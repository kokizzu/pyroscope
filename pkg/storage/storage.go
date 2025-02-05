package storage

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/pyroscope-io/pyroscope/pkg/util/disk"
	"github.com/pyroscope-io/pyroscope/pkg/util/metrics"

	"github.com/dgraph-io/badger/v2"
	"github.com/dgraph-io/badger/v2/options"
	"github.com/pyroscope-io/pyroscope/pkg/config"
	"github.com/pyroscope-io/pyroscope/pkg/storage/cache"
	"github.com/pyroscope-io/pyroscope/pkg/storage/dict"
	"github.com/pyroscope-io/pyroscope/pkg/storage/dimension"
	"github.com/pyroscope-io/pyroscope/pkg/storage/labels"
	"github.com/pyroscope-io/pyroscope/pkg/storage/segment"
	"github.com/pyroscope-io/pyroscope/pkg/storage/tree"
	"github.com/pyroscope-io/pyroscope/pkg/structs/merge"
	"github.com/pyroscope-io/pyroscope/pkg/util/bytesize"
	"github.com/pyroscope-io/pyroscope/pkg/util/slices"
	"github.com/sirupsen/logrus"
)

var ErrClosing = errors.New("the db is in closing state")
var ErrAlreadyClosed = errors.New("the db is already closed")
var errOutOfSpace = errors.New("running out of space")
var errRetention = errors.New("could not write because of retention settings")

var evictInterval = 1 * time.Second
var writeBackInterval = 1 * time.Second
var retentionInterval = 1 * time.Minute

type Storage struct {
	closingMutex sync.RWMutex
	closing      bool

	putMutex sync.Mutex

	cfg      *config.Server
	segments *cache.Cache

	dimensions *cache.Cache
	dicts      *cache.Cache
	trees      *cache.Cache
	labels     *labels.Labels

	db           *badger.DB
	dbTrees      *badger.DB
	dbDicts      *badger.DB
	dbDimensions *badger.DB
	dbSegments   *badger.DB

	localProfilesDir string
}

func badgerGC(db *badger.DB) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
	again:
		err := db.RunValueLogGC(0.7)
		if err == nil {
			goto again
		}
	}
}

func newBadger(cfg *config.Server, name string) (*badger.DB, error) {
	badgerPath := filepath.Join(cfg.StoragePath, name)
	err := os.MkdirAll(badgerPath, 0o755)
	if err != nil {
		return nil, err
	}
	badgerOptions := badger.DefaultOptions(badgerPath)
	badgerOptions = badgerOptions.WithTruncate(!cfg.BadgerNoTruncate)
	badgerOptions = badgerOptions.WithSyncWrites(false)
	badgerOptions = badgerOptions.WithCompactL0OnClose(false)
	badgerOptions = badgerOptions.WithCompression(options.ZSTD)
	badgerLevel := logrus.ErrorLevel
	if l, err := logrus.ParseLevel(cfg.BadgerLogLevel); err == nil {
		badgerLevel = l
	}
	badgerOptions = badgerOptions.WithLogger(badgerLogger{name: name, logLevel: badgerLevel})

	db, err := badger.Open(badgerOptions)
	if err == nil {
		go badgerGC(db)
	}
	return db, err
}

func New(cfg *config.Server) (*Storage, error) {
	db, err := newBadger(cfg, "main")
	if err != nil {
		return nil, err
	}
	dbTrees, err := newBadger(cfg, "trees")
	if err != nil {
		return nil, err
	}
	dbDicts, err := newBadger(cfg, "dicts")
	if err != nil {
		return nil, err
	}
	dbDimensions, err := newBadger(cfg, "dimensions")
	if err != nil {
		return nil, err
	}
	dbSegments, err := newBadger(cfg, "segments")
	if err != nil {
		return nil, err
	}

	localProfilesDir := filepath.Join(cfg.StoragePath, "local-profiles")
	if err = os.MkdirAll(localProfilesDir, 0o755); err != nil {
		return nil, err
	}

	s := &Storage{
		cfg:              cfg,
		labels:           labels.New(db),
		db:               db,
		dbTrees:          dbTrees,
		dbDicts:          dbDicts,
		dbDimensions:     dbDimensions,
		dbSegments:       dbSegments,
		localProfilesDir: localProfilesDir,
	}

	s.dimensions = cache.New(dbDimensions, "i:", "dimensions")
	s.dimensions.Bytes = func(k string, v interface{}) ([]byte, error) {
		return v.(*dimension.Dimension).Bytes()
	}
	s.dimensions.FromBytes = func(k string, v []byte) (interface{}, error) {
		return dimension.FromBytes(v)
	}
	s.dimensions.New = func(k string) interface{} {
		return dimension.New()
	}

	s.segments = cache.New(dbSegments, "s:", "segments")
	s.segments.Bytes = func(k string, v interface{}) ([]byte, error) {
		return v.(*segment.Segment).Bytes()
	}
	s.segments.FromBytes = func(k string, v []byte) (interface{}, error) {
		// TODO:
		//   these configuration params should be saved in db when it initializes
		return segment.FromBytes(v)
	}
	s.segments.New = func(k string) interface{} {
		return segment.New()
	}

	s.dicts = cache.New(dbDicts, "d:", "dicts")
	s.dicts.Bytes = func(k string, v interface{}) ([]byte, error) {
		return v.(*dict.Dict).Bytes()
	}
	s.dicts.FromBytes = func(k string, v []byte) (interface{}, error) {
		return dict.FromBytes(v)
	}
	s.dicts.New = func(k string) interface{} {
		return dict.New()
	}

	s.trees = cache.New(dbTrees, "t:", "trees")
	s.trees.Bytes = func(k string, v interface{}) ([]byte, error) {
		key := FromTreeToMainKey(k)
		d, err := s.dicts.Get(key)
		if err != nil {
			return nil, fmt.Errorf("dicts cache for %v: %v", key, err)
		}
		if d == nil { // key not found
			return nil, nil
		}
		return v.(*tree.Tree).Bytes(d.(*dict.Dict), cfg.MaxNodesSerialization)
	}
	s.trees.FromBytes = func(k string, v []byte) (interface{}, error) {
		key := FromTreeToMainKey(k)
		d, err := s.dicts.Get(FromTreeToMainKey(k))
		if err != nil {
			return nil, fmt.Errorf("dicts cache for %v: %v", key, err)
		}
		if d == nil { // key not found
			return nil, nil
		}
		return tree.FromBytes(d.(*dict.Dict), v)
	}
	s.trees.New = func(k string) interface{} {
		return tree.New()
	}

	if err := s.startEvictTimer(evictInterval); err != nil {
		return nil, fmt.Errorf("start evict timer: %v", err)
	}

	if err := s.startWriteBackTimer(writeBackInterval); err != nil {
		return nil, fmt.Errorf("start WriteBack timer: %v", err)
	}

	if err := s.startRetentionTimer(retentionInterval); err != nil {
		return nil, fmt.Errorf("start WriteBack timer: %v", err)
	}

	return s, nil
}

type PutInput struct {
	StartTime       time.Time
	EndTime         time.Time
	Key             *Key
	Val             *tree.Tree
	SpyName         string
	SampleRate      uint32
	Units           string
	AggregationType string
}

func (s *Storage) IsClosing() bool {
	s.closingMutex.RLock()
	defer s.closingMutex.RUnlock()

	return s.closing
}

var OutOfSpaceThreshold = 512 * bytesize.MB

func (s *Storage) Put(po *PutInput) error {
	s.closingMutex.RLock()
	defer s.closingMutex.RUnlock()
	if s.closing {
		return ErrClosing
	}

	// TODO: This is a pretty broad lock. We should find a way to make these locks more selective.
	s.putMutex.Lock()
	defer s.putMutex.Unlock()

	if err := s.performFreeSpaceCheck(); err != nil {
		return err
	}

	if po.StartTime.Before(s.lifetimeBasedRetentionThreshold()) {
		return errRetention
	}

	logrus.WithFields(logrus.Fields{
		"startTime":       po.StartTime.String(),
		"endTime":         po.EndTime.String(),
		"key":             po.Key.Normalized(),
		"samples":         po.Val.Samples(),
		"units":           po.Units,
		"aggregationType": po.AggregationType,
	}).Debug("storage.Put")

	for k, v := range po.Key.labels {
		s.labels.Put(k, v)
	}

	sk := po.Key.SegmentKey()
	for k, v := range po.Key.labels {
		key := k + ":" + v
		res, err := s.dimensions.Get(key)
		if err != nil {
			logrus.Errorf("dimensions cache for %v: %v", key, err)
			continue
		}
		if res != nil {
			res.(*dimension.Dimension).Insert([]byte(sk))
		}
	}

	res, err := s.segments.Get(sk)
	if err != nil {
		return fmt.Errorf("segments cache for %v: %v", sk, err)
	}
	if res == nil {
		return fmt.Errorf("segments cache for %v: not found", sk)
	}

	st := res.(*segment.Segment)
	st.SetMetadata(po.SpyName, po.SampleRate, po.Units, po.AggregationType)
	samples := po.Val.Samples()
	st.Put(po.StartTime, po.EndTime, samples, func(depth int, t time.Time, r *big.Rat, addons []segment.Addon) {
		tk := po.Key.TreeKey(depth, t)

		res, err := s.trees.Get(tk)
		if err != nil {
			logrus.Errorf("trees cache for %v: %v", tk, err)
			return
		}
		cachedTree := res.(*tree.Tree)

		treeClone := po.Val.Clone(r)
		for _, addon := range addons {
			tk2 := po.Key.TreeKey(addon.Depth, addon.T)

			res, err := s.trees.Get(tk2)
			if err != nil {
				logrus.Errorf("trees cache for %v: %v", tk, err)
				continue
			}
			if res == nil {
				continue
			}
			treeClone.Merge(res.(*tree.Tree))
		}
		if cachedTree != nil {
			cachedTree.Merge(treeClone)
			s.trees.Put(tk, cachedTree)
		} else {
			s.trees.Put(tk, treeClone)
		}
	})
	s.segments.Put(string(sk), st)

	return nil
}

type GetInput struct {
	StartTime time.Time
	EndTime   time.Time
	Key       *Key
}

type GetOutput struct {
	Tree       *tree.Tree
	Timeline   *segment.Timeline
	SpyName    string
	SampleRate uint32
	Units      string
}

func (s *Storage) Get(gi *GetInput) (*GetOutput, error) {
	s.closingMutex.RLock()
	defer s.closingMutex.RUnlock()
	if s.closing {
		return nil, ErrClosing
	}

	logrus.WithFields(logrus.Fields{
		"startTime": gi.StartTime.String(),
		"endTime":   gi.EndTime.String(),
		"key":       gi.Key.Normalized(),
	}).Trace("storage.Get")
	triesToMerge := []merge.Merger{}

	dimensions := []*dimension.Dimension{}
	for k, v := range gi.Key.labels {
		key := k + ":" + v
		res, err := s.dimensions.Get(key)
		if err != nil {
			logrus.Errorf("dimensions cache for %v: %v", key, err)
			continue
		}
		if res != nil {
			dimensions = append(dimensions, res.(*dimension.Dimension))
		}
	}

	segmentKeys := dimension.Intersection(dimensions...)

	tl := segment.GenerateTimeline(gi.StartTime, gi.EndTime)
	var lastSegment *segment.Segment
	var writesTotal uint64
	aggregationType := "sum"
	for _, sk := range segmentKeys {
		// TODO: refactor, store `Key`s in dimensions
		parsedKey, err := ParseKey(string(sk))
		if err != nil {
			logrus.Errorf("parse key: %v: %v", string(sk), err)
			continue
		}

		key := parsedKey.SegmentKey()
		res, err := s.segments.Get(key)
		if err != nil {
			logrus.Errorf("segments cache for %v: %v", key, err)
			continue
		}
		if res == nil {
			continue
		}

		st := res.(*segment.Segment)
		if st.AggregationType() == "average" {
			aggregationType = "average"
		}
		lastSegment = st

		tl.PopulateTimeline(st)

		st.Get(gi.StartTime, gi.EndTime, func(depth int, samples, writes uint64, t time.Time, r *big.Rat) {
			key := parsedKey.TreeKey(depth, t)
			res, err := s.trees.Get(key)
			if err != nil {
				logrus.Errorf("trees cache for %v: %v", key, err)
				return
			}

			tr := res.(*tree.Tree)
			// TODO: these clones are probably are not the most efficient way of doing this
			//   instead this info should be passed to the merger function imo
			tr2 := tr.Clone(r)
			triesToMerge = append(triesToMerge, merge.Merger(tr2))
			writesTotal += writes
		})
	}

	resultTrie := merge.MergeTriesConcurrently(runtime.NumCPU(), triesToMerge...)
	if resultTrie == nil {
		return nil, nil
	}

	t := resultTrie.(*tree.Tree)

	if writesTotal > 0 && aggregationType == "average" {
		t = t.Clone(big.NewRat(1, int64(writesTotal)))
	}

	return &GetOutput{
		Tree:       t,
		Timeline:   tl,
		SpyName:    lastSegment.SpyName(),
		SampleRate: lastSegment.SampleRate(),
		Units:      lastSegment.Units(),
	}, nil
}

func (s *Storage) iterateOverAllSegments(cb func(*Key, *segment.Segment) error) error {
	nameKey := "__name__"

	var dimensions []*dimension.Dimension
	var err error
	s.labels.GetValues(nameKey, func(v string) bool {
		dmInt, getErr := s.dimensions.Get(nameKey + ":" + v)
		dm, _ := dmInt.(*dimension.Dimension)
		err = getErr
		dimensions = append(dimensions, dm)
		return err == nil
	})

	if err != nil {
		return err
	}

	segmentKeys := dimension.Union(dimensions...)

	for _, rawSk := range segmentKeys {
		sk, _ := ParseKey(string(rawSk))

		stInt, err := s.segments.Get(sk.SegmentKey())
		if err != nil {
			return err
		}
		st := stInt.(*segment.Segment)
		if err := cb(sk, st); err != nil {
			return err
		}
	}
	return nil
}

func (s *Storage) DeleteDataBefore(threshold time.Time) error {
	s.closingMutex.RLock()
	defer s.closingMutex.RUnlock()
	if s.closing {
		return ErrClosing
	}

	return s.iterateOverAllSegments(func(sk *Key, st *segment.Segment) error {
		var err error
		deletedRoot := st.DeleteDataBefore(threshold, func(depth int, t time.Time) {
			tk := sk.TreeKey(depth, t)
			if delErr := s.trees.Delete(tk); delErr != nil {
				err = delErr
			}
		})
		if err != nil {
			return err
		}

		if deletedRoot {
			s.deleteSegmentAndRelatedData(sk)
		}
		return nil
	})
}

type DeleteInput struct {
	Key *Key
}

var maxTime = time.Unix(1<<62, 999999999)

func (s *Storage) Delete(di *DeleteInput) error {
	s.closingMutex.RLock()
	defer s.closingMutex.RUnlock()
	if s.closing {
		return ErrClosing
	}

	dimensions := []*dimension.Dimension{}
	for k, v := range di.Key.labels {
		dInt, err := s.dimensions.Get(k + ":" + v)
		if err != nil {
			return nil
		}
		d := dInt.(*dimension.Dimension)
		dimensions = append(dimensions, d)
	}

	segmentKeys := dimension.Intersection(dimensions...)

	for _, sk := range segmentKeys {
		skk, _ := ParseKey(string(sk))
		stInt, err := s.segments.Get(skk.SegmentKey())
		if err != nil {
			return nil
		}
		st := stInt.(*segment.Segment)
		if st == nil {
			continue
		}

		st.Get(zeroTime, maxTime, func(depth int, _, _ uint64, t time.Time, _ *big.Rat) {
			treeKey := skk.TreeKey(depth, t)
			err = s.trees.Delete(treeKey)
		})
		if err != nil {
			return err
		}

		s.deleteSegmentAndRelatedData(skk)
	}

	return nil
}

func (s *Storage) deleteSegmentAndRelatedData(key *Key) error {
	s.dicts.Delete(key.DictKey())
	s.segments.Delete(key.SegmentKey())

	for k, v := range key.labels {
		dInt, err := s.dimensions.Get(k + ":" + v)
		if err != nil {
			return err
		}
		d := dInt.(*dimension.Dimension)
		d.Delete(dimension.Key(key.SegmentKey()))
	}
	return nil
}

func (s *Storage) Close() error {
	if s.IsClosing() {
		return ErrAlreadyClosed
	}

	s.closingMutex.Lock()
	s.closing = true
	s.closingMutex.Unlock()

	metrics.Timing("storage_caches_flush_timer", func() {
		wg := sync.WaitGroup{}
		wg.Add(3)
		go func() { defer wg.Done(); s.dimensions.Flush() }()
		go func() { defer wg.Done(); s.segments.Flush() }()
		go func() { defer wg.Done(); s.trees.Flush() }()
		wg.Wait()

		// dictionary has to flush last because trees write to dictionaries
		s.dicts.Flush()
	})

	metrics.Timing("storage_badger_close_timer", func() {
		wg := sync.WaitGroup{}
		wg.Add(5)
		go func() { defer wg.Done(); s.dbTrees.Close() }()
		go func() { defer wg.Done(); s.dbDicts.Close() }()
		go func() { defer wg.Done(); s.dbDimensions.Close() }()
		go func() { defer wg.Done(); s.dbSegments.Close() }()
		go func() { defer wg.Done(); s.db.Close() }()
		wg.Wait()
	})
	// this allows prometheus to collect metrics before pyroscope exits
	if os.Getenv("PYROSCOPE_WAIT_AFTER_STOP") != "" {
		time.Sleep(5 * time.Second)
	}
	return nil
}

func (s *Storage) GetKeys(cb func(_k string) bool) {
	s.labels.GetKeys(cb)
}

func (s *Storage) GetValues(key string, cb func(v string) bool) {
	s.labels.GetValues(key, func(v string) bool {
		if key != "__name__" || !slices.StringContains(s.cfg.HideApplications, v) {
			return cb(v)
		}
		return true
	})
}

func (s *Storage) DiskUsage() map[string]bytesize.ByteSize {
	res := map[string]bytesize.ByteSize{
		"main":       0,
		"trees":      0,
		"dicts":      0,
		"dimensions": 0,
		"segments":   0,
	}
	for k := range res {
		res[k] = dirSize(filepath.Join(s.cfg.StoragePath, k))
	}
	return res
}

func dirSize(path string) (result bytesize.ByteSize) {
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			result += bytesize.ByteSize(info.Size())
		}
		return nil
	})
	return result
}

func (s *Storage) CacheStats() map[string]interface{} {
	return map[string]interface{}{
		"dimensions_size": s.dimensions.Size(),
		"segments_size":   s.segments.Size(),
		"dicts_size":      s.dicts.Size(),
		"trees_size":      s.trees.Size(),
	}
}

var zeroTime time.Time

func (s *Storage) lifetimeBasedRetentionThreshold() time.Time {
	var t time.Time
	if s.cfg.Retention != 0 {
		t = time.Now().Add(-1 * s.cfg.Retention)
	}
	return t
}

func (s *Storage) performFreeSpaceCheck() error {
	freeSpace, err := disk.FreeSpace(s.cfg.StoragePath)
	if err == nil {
		if freeSpace < OutOfSpaceThreshold {
			return errOutOfSpace
		}
	}
	return nil
}
