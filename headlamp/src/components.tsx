import { Icon } from '@iconify/react';
import { SectionBox } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { Box, Button, Link, Typography } from '@mui/material';
import {
  consoleClusterURL,
  consoleURLProblemText,
  isCnpgCluster,
  phaseBadge,
  rawObject,
} from './cluster';
import { store } from './config';

const useConfig = store.useConfig();

function useClusterConsoleURL(item: unknown) {
  const config = useConfig();
  const obj = rawObject(item);
  return {
    obj,
    result: consoleClusterURL(config?.consoleURL, obj?.metadata?.namespace, obj?.metadata?.name),
  };
}

/** Header action on a CNPG Cluster details view: a real link, opened in a new tab. */
export function OpenInConsoleAction({ item }: { item: unknown }) {
  const { obj, result } = useClusterConsoleURL(item);
  if (!isCnpgCluster(obj)) {
    return null;
  }
  if (!result.ok) {
    const descId = 'pgtoolbox-console-action-problem';
    return (
      <Box sx={{ display: 'inline-flex', alignItems: 'center', gap: 1 }}>
        <Button
          size="small"
          variant="outlined"
          disabled
          aria-describedby={descId}
          startIcon={<Icon icon="mdi:open-in-new" aria-hidden />}
        >
          Open in the pgToolBox console
        </Button>
        <Typography id={descId} variant="caption">
          {consoleURLProblemText(result.problem)}
        </Typography>
      </Box>
    );
  }
  return (
    <Button
      size="small"
      variant="outlined"
      component="a"
      href={result.url}
      target="_blank"
      rel="noopener noreferrer"
      startIcon={<Icon icon="mdi:open-in-new" aria-hidden />}
    >
      Open in the pgToolBox console
    </Button>
  );
}

/** Text-only phase badge: glyph + words, never colour alone, no verdict. */
export function PhaseBadgeText({ item }: { item: unknown }) {
  const badge = phaseBadge(rawObject(item));
  return (
    <Box component="span" sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.5 }}>
      <Icon icon="mdi:information-outline" aria-hidden />
      <span>{badge.phase ?? 'not reported'}</span>
    </Box>
  );
}

/** Details-view section: the phase as reported by the operator, and a link to the verdict. */
export function OperatorPhaseSection({ resource }: { resource: unknown }) {
  const { obj, result } = useClusterConsoleURL(resource);
  if (!isCnpgCluster(obj)) {
    return null;
  }
  const badge = phaseBadge(obj);
  return (
    <SectionBox title="CloudNativePG phase">
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        <Icon icon="mdi:information-outline" aria-hidden />
        <Typography component="span" sx={{ fontWeight: 'bold' }}>
          {badge.text}
        </Typography>
      </Box>
      {badge.instancesText !== null && (
        <Typography variant="body2">{badge.instancesText}</Typography>
      )}
      <Typography variant="body2" sx={{ mt: 1 }}>
        Phase as reported by the operator in status.phase; it is not a health verdict.{' '}
        {result.ok ? (
          <Link href={result.url} target="_blank" rel="noopener noreferrer">
            See the health verdict in the pgToolBox console
          </Link>
        ) : (
          <>The health verdict is in the pgToolBox console: {consoleURLProblemText(result.problem)}.</>
        )}
      </Typography>
    </SectionBox>
  );
}
