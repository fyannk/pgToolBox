/*
 * Pure helpers for the pgToolBox Headlamp plugin. No React, no Headlamp
 * imports: everything here is unit-tested in cluster.test.ts.
 *
 * This plugin never derives a health verdict. The verdict lives in
 * the console's Go backend; here we only echo what the CloudNativePG operator
 * wrote into the Cluster object and link to the console.
 */

/** API group of CloudNativePG resources. */
export const CNPG_GROUP = 'postgresql.cnpg.io';
/** Kind of the CloudNativePG cluster resource. */
export const CNPG_CLUSTER_KIND = 'Cluster';

/** The subset of a CloudNativePG Cluster object this plugin reads. */
export interface CnpgClusterJSON {
  apiVersion?: string;
  kind?: string;
  metadata?: { name?: string; namespace?: string };
  status?: { phase?: unknown; instances?: unknown; readyInstances?: unknown };
}

/**
 * Returns the raw JSON of a Headlamp KubeObject (its `jsonData`), or the value
 * itself when it is already plain JSON.
 */
export function rawObject(item: unknown): CnpgClusterJSON | null {
  if (!item || typeof item !== 'object') {
    return null;
  }
  const withJSON = item as { jsonData?: unknown };
  if (withJSON.jsonData && typeof withJSON.jsonData === 'object') {
    return withJSON.jsonData as CnpgClusterJSON;
  }
  return item as CnpgClusterJSON;
}

/** True when the object is a postgresql.cnpg.io Cluster (any API version). */
export function isCnpgCluster(obj: CnpgClusterJSON | null): boolean {
  if (!obj || obj.kind !== CNPG_CLUSTER_KIND || typeof obj.apiVersion !== 'string') {
    return false;
  }
  return obj.apiVersion.split('/')[0] === CNPG_GROUP;
}

/** Why a console URL could not be built. */
export type ConsoleURLProblem = 'unset' | 'invalid' | 'no-target';

export type ConsoleURLResult =
  | { ok: true; url: string }
  | { ok: false; problem: ConsoleURLProblem };

/**
 * Validates the configured console base URL. Accepts absolute http(s) URLs
 * only, without credentials, query or fragment. Returns the base without
 * trailing slashes, or the problem.
 */
export function normalizeBaseURL(
  base: string | undefined | null
): { ok: true; base: string } | { ok: false; problem: 'unset' | 'invalid' } {
  const trimmed = (base ?? '').trim();
  if (trimmed === '') {
    return { ok: false, problem: 'unset' };
  }
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return { ok: false, problem: 'invalid' };
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return { ok: false, problem: 'invalid' };
  }
  if (
    parsed.username !== '' ||
    parsed.password !== '' ||
    parsed.search !== '' ||
    parsed.hash !== ''
  ) {
    return { ok: false, problem: 'invalid' };
  }
  const path = parsed.pathname.replace(/\/+$/, '');
  return { ok: true, base: `${parsed.origin}${path}` };
}

/**
 * Builds `<base>/c/<namespace>/<cluster>`, the console page of one Cluster.
 * Each path segment is percent-encoded.
 */
export function consoleClusterURL(
  base: string | undefined | null,
  namespace: string | undefined | null,
  name: string | undefined | null
): ConsoleURLResult {
  const normalized = normalizeBaseURL(base);
  if (!normalized.ok) {
    return normalized;
  }
  if (!namespace || !name) {
    return { ok: false, problem: 'no-target' };
  }
  return {
    ok: true,
    url: `${normalized.base}/c/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`,
  };
}

/** Human text for a URL problem, shown next to the disabled action. */
export function consoleURLProblemText(problem: ConsoleURLProblem): string {
  switch (problem) {
    case 'unset':
      return 'pgToolBox console URL is not configured in the plugin settings';
    case 'invalid':
      return 'pgToolBox console URL in the plugin settings is not a valid http(s) URL';
    case 'no-target':
      return 'this object has no namespace or name';
  }
}

/** What the badge shows: the operator-reported phase and instance counts, verbatim. */
export interface PhaseBadge {
  /** status.phase exactly as reported, or null when absent. */
  phase: string | null;
  /** One line: "Phase: <phase>" or "Phase: not reported". */
  text: string;
  /** "<ready>/<instances> instances ready", or null when the operator reports neither. */
  instancesText: string | null;
}

function count(value: unknown): number | null {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 ? value : null;
}

/** Builds the badge from the Cluster object as fetched. Derives no verdict. */
export function phaseBadge(obj: CnpgClusterJSON | null): PhaseBadge {
  const status = obj?.status;
  const rawPhase = status?.phase;
  const phase = typeof rawPhase === 'string' && rawPhase.trim() !== '' ? rawPhase : null;
  const text = phase === null ? 'Phase: not reported' : `Phase: ${phase}`;

  const ready = count(status?.readyInstances);
  const instances = count(status?.instances);
  let instancesText: string | null = null;
  if (instances !== null) {
    instancesText =
      ready !== null
        ? `${ready}/${instances} instances ready`
        : `${instances} instances, ready count not reported`;
  } else if (ready !== null) {
    instancesText = `${ready} instances ready, total not reported`;
  }
  return { phase, text, instancesText };
}
