import {
  registerDetailsViewHeaderAction,
  registerDetailsViewSection,
  registerPluginSettings,
  registerResourceTableColumnsProcessor,
} from '@kinvolk/headlamp-plugin/lib';
import { isCnpgCluster, phaseBadge, rawObject } from './cluster';
import { OpenInConsoleAction, OperatorPhaseSection, PhaseBadgeText } from './components';
import { PLUGIN_NAME } from './config';
import Settings from './settings';

registerDetailsViewHeaderAction(OpenInConsoleAction);

registerDetailsViewSection(({ resource }) => <OperatorPhaseSection resource={resource} />);

// Headlamp's generic custom-resource list derives its table id from the plural
// name ("headlamp-clusters"), which other CRDs named "clusters" share, so each
// row is checked for the CNPG group before a value is shown.
registerResourceTableColumnsProcessor(function cnpgPhaseColumn({ id, columns }) {
  if (id !== 'headlamp-clusters') {
    return columns;
  }
  if (
    columns.some(
      c => typeof c === 'object' && c !== null && 'id' in c && c.id === `${PLUGIN_NAME}-phase`
    )
  ) {
    return columns;
  }
  columns.push({
    id: `${PLUGIN_NAME}-phase`,
    label: 'Operator phase',
    getValue: (item: unknown) => {
      const obj = rawObject(item);
      return isCnpgCluster(obj) ? phaseBadge(obj).phase ?? 'not reported' : '';
    },
    render: (item: unknown) =>
      isCnpgCluster(rawObject(item)) ? <PhaseBadgeText item={item} /> : null,
  });
  return columns;
});

registerPluginSettings(PLUGIN_NAME, Settings, true);
