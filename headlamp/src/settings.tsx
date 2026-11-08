import { PluginSettingsDetailsProps } from '@kinvolk/headlamp-plugin/lib';
import { Box, TextField, Typography } from '@mui/material';
import { consoleURLProblemText, normalizeBaseURL } from './cluster';
import { PluginConfig } from './config';

/** Settings page: Settings → Plugins → pgtoolbox-console. */
export default function Settings({ data, onDataChange }: PluginSettingsDetailsProps) {
  const config = (data ?? {}) as PluginConfig;
  const value = config.consoleURL ?? '';
  const check = normalizeBaseURL(value);
  const error = !check.ok && check.problem === 'invalid';
  const helper = check.ok
    ? `Cluster pages open at ${check.base}/c/<namespace>/<cluster>`
    : consoleURLProblemText(check.problem);

  return (
    <Box sx={{ p: 2 }}>
      <Typography variant="body2" sx={{ mb: 2 }}>
        Base URL of the pgToolBox console that serves this cluster&apos;s team namespaces. The plugin never
        guesses it.
      </Typography>
      <TextField
        id="pgtoolbox-console-url"
        label="pgToolBox console URL"
        placeholder="https://console.example.com"
        value={value}
        onChange={e => onDataChange?.({ ...config, consoleURL: e.target.value })}
        error={error}
        helperText={helper}
        type="url"
        fullWidth
        variant="outlined"
      />
    </Box>
  );
}
