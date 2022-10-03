import React from 'react';
import { DataSourcePluginOptionsEditorProps } from '@grafana/data';
import { FireDataSourceOptions } from './types';
import { DataSourceHttpSettings, EventsWithValidation, LegacyForms, regexValidation } from '@grafana/ui';

interface Props extends DataSourcePluginOptionsEditorProps<FireDataSourceOptions> {}

export const ConfigEditor = (props: Props) => {
  const { options, onOptionsChange } = props;

  return (
    <>
      <DataSourceHttpSettings
        defaultUrl={'http://localhost:4100'}
        dataSourceConfig={options}
        showAccessOptions={false}
        onChange={onOptionsChange}
      />

      <h3 className="page-heading">Querying</h3>
      <div className="gf-form-group">
        <div className="gf-form-inline">
          <div className="gf-form">
            <LegacyForms.FormField
              label="Minimal step"
              labelWidth={13}
              inputEl={
                <LegacyForms.Input
                  className="width-6"
                  value={options.jsonData.minStep}
                  spellCheck={false}
                  placeholder="15s"
                  onChange={(event) => {
                    onOptionsChange({
                      ...options,
                      jsonData: {
                        ...options.jsonData,
                        minStep: event.currentTarget.value,
                      },
                    });
                  }}
                  validationEvents={{
                    [EventsWithValidation.onBlur]: [
                      regexValidation(
                        /^$|^\d+(ms|[Mwdhmsy])$/,
                        'Value is not valid, you can use number with time unit specifier: y, M, w, d, h, m, s'
                      ),
                    ],
                  }}
                />
              }
              tooltip="Minimal step used for metric query. Should be the same or higher as the scrape interval setting in the Fire database."
            />
          </div>
        </div>
      </div>
    </>
  );
};
