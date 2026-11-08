import {
  consoleClusterURL,
  consoleURLProblemText,
  isCnpgCluster,
  normalizeBaseURL,
  phaseBadge,
  rawObject,
} from './cluster';

const cluster = (status?: Record<string, unknown>) => ({
  apiVersion: 'postgresql.cnpg.io/v1',
  kind: 'Cluster',
  metadata: { name: 'pg-main', namespace: 'team-a' },
  ...(status ? { status } : {}),
});

describe('normalizeBaseURL', () => {
  it.each([
    ['https://console.example.com', 'https://console.example.com'],
    ['https://console.example.com/', 'https://console.example.com'],
    ['https://console.example.com///', 'https://console.example.com'],
    ['  http://localhost:8080/  ', 'http://localhost:8080'],
    ['https://example.com/pgtoolbox/', 'https://example.com/pgtoolbox'],
  ])('accepts %s', (input, want) => {
    expect(normalizeBaseURL(input)).toEqual({ ok: true, base: want });
  });

  it.each([undefined, null, '', '   '])('reports unset for %s', input => {
    expect(normalizeBaseURL(input)).toEqual({ ok: false, problem: 'unset' });
  });

  it.each([
    'console.example.com',
    '/c/ns',
    'javascript:alert(1)',
    'ftp://example.com',
    'https://user:pw@example.com',
    'https://example.com/?a=1',
    'https://example.com/#x',
    'https://',
  ])('rejects %s as invalid', input => {
    expect(normalizeBaseURL(input)).toEqual({ ok: false, problem: 'invalid' });
  });
});

describe('consoleClusterURL', () => {
  it('builds <base>/c/<ns>/<name>', () => {
    expect(consoleClusterURL('https://console.example.com/', 'team-a', 'pg-main')).toEqual({
      ok: true,
      url: 'https://console.example.com/c/team-a/pg-main',
    });
  });

  it('keeps a sub-path base', () => {
    expect(consoleClusterURL('https://example.com/pgtoolbox', 'team-a', 'pg-main')).toEqual({
      ok: true,
      url: 'https://example.com/pgtoolbox/c/team-a/pg-main',
    });
  });

  it('percent-encodes each segment', () => {
    const r = consoleClusterURL('https://x.example', 'a/b', 'c d?#%');
    expect(r).toEqual({ ok: true, url: 'https://x.example/c/a%2Fb/c%20d%3F%23%25' });
  });

  it('reports unset and invalid bases without guessing a URL', () => {
    expect(consoleClusterURL(undefined, 'team-a', 'pg-main')).toEqual({
      ok: false,
      problem: 'unset',
    });
    expect(consoleClusterURL('nope', 'team-a', 'pg-main')).toEqual({
      ok: false,
      problem: 'invalid',
    });
  });

  it('reports a missing namespace or name', () => {
    expect(consoleClusterURL('https://x.example', undefined, 'pg-main')).toEqual({
      ok: false,
      problem: 'no-target',
    });
    expect(consoleClusterURL('https://x.example', 'team-a', '')).toEqual({
      ok: false,
      problem: 'no-target',
    });
  });

  it('explains each problem in words', () => {
    expect(consoleURLProblemText('unset')).toBe(
      'pgToolBox console URL is not configured in the plugin settings'
    );
    expect(consoleURLProblemText('invalid')).toMatch(/not a valid http\(s\) URL/);
    expect(consoleURLProblemText('no-target')).toMatch(/no namespace or name/);
  });
});

describe('isCnpgCluster / rawObject', () => {
  it('matches postgresql.cnpg.io Cluster from plain JSON or a KubeObject', () => {
    expect(isCnpgCluster(rawObject(cluster()))).toBe(true);
    expect(isCnpgCluster(rawObject({ jsonData: cluster() }))).toBe(true);
  });

  it('ignores other Cluster kinds and other objects', () => {
    expect(
      isCnpgCluster(rawObject({ apiVersion: 'cluster.x-k8s.io/v1beta1', kind: 'Cluster' }))
    ).toBe(false);
    expect(isCnpgCluster(rawObject({ apiVersion: 'postgresql.cnpg.io/v1', kind: 'Pooler' }))).toBe(
      false
    );
    expect(isCnpgCluster(rawObject(null))).toBe(false);
  });
});

describe('phaseBadge', () => {
  it('shows the phase exactly as the operator reports it', () => {
    const b = phaseBadge(
      cluster({ phase: 'Cluster in healthy state', instances: 3, readyInstances: 3 })
    );
    expect(b.phase).toBe('Cluster in healthy state');
    expect(b.text).toBe('Phase: Cluster in healthy state');
    expect(b.instancesText).toBe('3/3 instances ready');
  });

  it('does not reinterpret an unhealthy-looking phase', () => {
    const b = phaseBadge(
      cluster({
        phase: 'Waiting for the instances to become active',
        instances: 3,
        readyInstances: 1,
      })
    );
    expect(b.text).toBe('Phase: Waiting for the instances to become active');
    expect(b.instancesText).toBe('1/3 instances ready');
  });

  it('says "not reported" when status.phase is absent', () => {
    expect(phaseBadge(cluster())).toEqual({
      phase: null,
      text: 'Phase: not reported',
      instancesText: null,
    });
    expect(phaseBadge(cluster({ phase: '' })).text).toBe('Phase: not reported');
    expect(phaseBadge(null).text).toBe('Phase: not reported');
  });

  it('reports partial instance counts honestly', () => {
    expect(phaseBadge(cluster({ instances: 2 })).instancesText).toBe(
      '2 instances, ready count not reported'
    );
    expect(phaseBadge(cluster({ readyInstances: 0 })).instancesText).toBe(
      '0 instances ready, total not reported'
    );
    expect(
      phaseBadge(cluster({ instances: 'three', readyInstances: -1 })).instancesText
    ).toBeNull();
  });

  it('reads readyInstances of 0 as a count, not as absent', () => {
    expect(
      phaseBadge(cluster({ phase: 'Setting up primary', instances: 1, readyInstances: 0 }))
        .instancesText
    ).toBe('0/1 instances ready');
  });
});
