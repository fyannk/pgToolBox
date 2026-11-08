import { ConfigStore } from '@kinvolk/headlamp-plugin/lib';

/** Plugin name; must match package.json "name" and registerPluginSettings. */
export const PLUGIN_NAME = 'pgtoolbox-console';

/** Settings stored by Headlamp for this plugin. */
export interface PluginConfig {
  /** Base URL of the team's pgToolBox console, e.g. https://console.team-a.example.com */
  consoleURL?: string;
}

export const store = new ConfigStore<PluginConfig>(PLUGIN_NAME);
