import React, {useEffect, useState} from 'react';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';

/*
 * The Security page. Its data is results.json, which the Pages workflow writes
 * into build/security/ after the site is built (hack/security_report.py
 * render). That file is the page's view model: every label, status and
 * listing decision is taken in Python and tested by `make test-hack`; this
 * page only lays it out. The shape is documented beside RESULTS_SCHEMA in
 * hack/security_report.py.
 */

const SCHEMA = 2;
const POLICY_URL = 'https://github.com/fyannk/pgToolBox/security/policy';
// Not a route of the site: written next to the page after the build, so the
// broken-link checker must not look for it.
const RESULTS_HREF = 'pathname:///security/results.json';

type Status = 'ok' | 'not-run' | 'failed';
type Severity = 'critical' | 'high' | 'medium' | 'low' | 'unknown';
type Counts = Record<Severity, number>;

interface ScannerRow {
  scanner: string;
  title: string;
  target: string;
  name: string;
  tool: {name: string; version: string};
  status: Status;
  status_label: string;
  reason: string;
  scanned_at: string;
  commit: string;
  commit_url: string;
  counts: Counts | null;
  fixable: number | null;
}

interface ImageRow {
  image: string;
  target: string;
  status: Status;
  status_label: string;
  digest: string;
  image_id: string;
  commit: string;
  commit_url: string;
}

interface FindingRow {
  severity: Severity;
  scanner: string;
  target: string;
  id: string;
  url: string;
  title: string;
  location: string;
  version: string;
  fix: {state: 'fixed' | 'none' | 'unknown' | 'n/a' | 'accepted'; label: string; fixed_in: string[]};
}

interface Results {
  schema: number;
  generated_at: string;
  commit: string;
  commit_url: string;
  run_url: string;
  published: boolean;
  missing: {name: string; status: Status; status_label: string; reason: string}[];
  scanners: ScannerRow[];
  images: ImageRow[];
  scorecard: {
    status: Status;
    status_label: string;
    reason: string;
    score: number | string | null;
    date: string;
    version: string;
    commit: string;
    commit_url: string;
    report_url: string;
    checks: {name: string; score: number | string; reason: string}[];
  };
  findings: {
    state: 'no-results' | 'none-open' | 'listed';
    ran: number;
    total: number;
    omitted: number;
    rows: FindingRow[];
  };
  accepted: {scanner: string; id: string; package: string; reason: string; matched: boolean}[];
}

type Load =
  | {kind: 'loading'}
  | {kind: 'absent'}
  | {kind: 'error'; reason: string}
  | {kind: 'ready'; results: Results};

// The design's status glyphs: healthy ●, failing ■, unknown ○ (never colour alone).
const GLYPH: Record<Status, string> = {ok: '●', 'not-run': '○', failed: '■'};

function StatusWord({status, label}: {status: Status; label: string}): React.ReactElement {
  return (
    <span className={`ptb-sec__status ptb-sec__status--${status}`}>
      <span aria-hidden="true">{GLYPH[status] ?? GLYPH.failed}</span> {label}
    </span>
  );
}

function Commit({commit, url}: {commit: string; url: string}): React.ReactElement {
  if (!commit) {
    return <span className="ptb-sec__muted">unknown</span>;
  }
  const short = <code>{commit.slice(0, 12)}</code>;
  return url ? <Link href={url}>{short}</Link> : short;
}

function Table({caption, children}: {caption: string; children: React.ReactNode}): React.ReactElement {
  return (
    // A labelled, focusable region so a keyboard user can scroll a wide table.
    <div className="ptb-sec__scroll" role="region" aria-label={caption} tabIndex={0}>
      <table className="ptb-sec__table">
        <caption className="ptb-sec__caption">{caption}</caption>
        {children}
      </table>
    </div>
  );
}

function useResults(url: string): Load {
  const [state, setState] = useState<Load>({kind: 'loading'});
  useEffect(() => {
    const controller = new AbortController();
    fetch(url, {cache: 'no-cache', signal: controller.signal})
      .then(async (response) => {
        if (response.status === 404) {
          setState({kind: 'absent'});
          return;
        }
        if (!response.ok) {
          setState({kind: 'error', reason: `results.json answered HTTP ${response.status}`});
          return;
        }
        const doc = (await response.json()) as Results;
        if (doc?.schema !== SCHEMA) {
          setState({
            kind: 'error',
            reason: `results.json has schema ${String(doc?.schema)}; this page reads schema ${SCHEMA}`,
          });
          return;
        }
        setState(doc.published ? {kind: 'ready', results: doc} : {kind: 'absent'});
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) {
          return;
        }
        setState({kind: 'error', reason: `results.json could not be read (${String(err)})`});
      });
    return () => controller.abort();
  }, [url]);
  return state;
}

function Scanners({rows}: {rows: ScannerRow[]}): React.ReactElement {
  return (
    <Table caption="Every scanner of the run, with its result">
      <thead>
        <tr>
          <th scope="col">Scanner</th>
          <th scope="col">Tool</th>
          <th scope="col">Status</th>
          <th scope="col">Scanned</th>
          <th scope="col">Commit</th>
          <th scope="col" className="ptb-sec__n">Critical</th>
          <th scope="col" className="ptb-sec__n">High</th>
          <th scope="col" className="ptb-sec__n">Medium</th>
          <th scope="col" className="ptb-sec__n">Low</th>
          <th scope="col" className="ptb-sec__n">Unknown</th>
          <th scope="col" className="ptb-sec__n">With a fix</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((row) => {
          const c = row.counts;
          const cell = (value: number | null | undefined) => (
            <td className="ptb-sec__n">
              {value === null || value === undefined ? <span className="ptb-sec__muted">n/a</span> : value}
            </td>
          );
          return (
            <tr key={`${row.scanner}/${row.target}`}>
              <th scope="row">
                {row.title}
                {row.target ? (
                  <>
                    {' '}
                    <code>{row.target}</code>
                  </>
                ) : null}
              </th>
              <td>{`${row.tool.name} ${row.tool.version}`.trim()}</td>
              <td>
                <StatusWord status={row.status} label={row.status_label} />
                {row.status !== 'ok' && row.reason ? (
                  <div className="ptb-sec__muted">{row.reason}</div>
                ) : null}
              </td>
              <td className="ptb-sec__nowrap">{row.scanned_at}</td>
              <td>
                <Commit commit={row.commit} url={row.commit_url} />
              </td>
              {cell(c?.critical)}
              {cell(c?.high)}
              {cell(c?.medium)}
              {cell(c?.low)}
              {cell(c?.unknown)}
              {cell(row.fixable)}
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}

function Images({rows}: {rows: ImageRow[]}): React.ReactElement {
  if (rows.length === 0) {
    return <p className="ptb-sec__muted">No image scan is part of this run.</p>;
  }
  const na = <span className="ptb-sec__muted">n/a</span>;
  return (
    <>
      <Table caption="Container images scanned, by digest">
        <thead>
          <tr>
            <th scope="col">Image</th>
            <th scope="col">Status</th>
            <th scope="col">Digest</th>
            <th scope="col">Image ID</th>
            <th scope="col">Commit</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.target || row.image}>
              <th scope="row">
                <code>{row.image}</code>
              </th>
              <td>
                <StatusWord status={row.status} label={row.status_label} />
              </td>
              <td>{row.digest ? <code className="ptb-sec__break">{row.digest}</code> : na}</td>
              <td>{row.image_id ? <code className="ptb-sec__break">{row.image_id}</code> : na}</td>
              <td>
                <Commit commit={row.commit} url={row.commit_url} />
              </td>
            </tr>
          ))}
        </tbody>
      </Table>
      <p className="ptb-sec__muted">
        Images built from the commit above are scanned before anything is published. The published
        release images are scanned by digest when a release exists; before the first release that row
        reads &ldquo;not run&rdquo;.
      </p>
    </>
  );
}

function Scorecard({sc}: {sc: Results['scorecard']}): React.ReactElement {
  if (sc.status !== 'ok') {
    return (
      <p>
        <StatusWord status={sc.status} label={sc.status_label} />{' '}
        <span className="ptb-sec__muted">{sc.reason}</span>
      </p>
    );
  }
  return (
    <>
      <p>
        <strong className="ptb-sec__score">{String(sc.score)} / 10</strong>{' '}
        <span className="ptb-sec__muted">
          (scorecard {sc.version}, {sc.date}, commit <Commit commit={sc.commit} url={sc.commit_url} />)
        </span>
        {sc.report_url ? (
          <>
            {' '}
            <Link href={sc.report_url}>Full report</Link>
          </>
        ) : null}
      </p>
      {sc.checks.length > 0 ? (
        <Table caption="OpenSSF Scorecard checks">
          <thead>
            <tr>
              <th scope="col">Check</th>
              <th scope="col" className="ptb-sec__n">Score</th>
              <th scope="col">Reason</th>
            </tr>
          </thead>
          <tbody>
            {sc.checks.map((check) => (
              <tr key={check.name}>
                <th scope="row">{check.name}</th>
                <td className="ptb-sec__n">{String(check.score)}</td>
                <td>{check.reason}</td>
              </tr>
            ))}
          </tbody>
        </Table>
      ) : null}
    </>
  );
}

function Findings({f}: {f: Results['findings']}): React.ReactElement {
  if (f.state === 'no-results') {
    return <p className="ptb-sec__muted">No scanner produced a result, so there is nothing to list.</p>;
  }
  if (f.state === 'none-open') {
    return (
      <p>
        The {f.ran} scanner(s) that ran report no open findings. Scanners that did not run are listed
        above and are not part of this statement.
      </p>
    );
  }
  return (
    <>
      <p className="ptb-sec__muted">
        {f.total} open finding(s), most severe first. Findings without a fix stay listed.
      </p>
      <Table caption="Open findings, with whether a fix exists">
        <thead>
          <tr>
            <th scope="col">Severity</th>
            <th scope="col">Scanner</th>
            <th scope="col">Target</th>
            <th scope="col">Finding</th>
            <th scope="col">Package or location</th>
            <th scope="col">Installed</th>
            <th scope="col">Fix available</th>
          </tr>
        </thead>
        <tbody>
          {f.rows.map((row, index) => (
            <tr key={`${row.scanner}/${row.target}/${row.id}/${row.location}/${index}`}>
              <td>
                <span className={`ptb-sec__sev ptb-sec__sev--${row.severity}`}>{row.severity}</span>
              </td>
              <td>{row.scanner}</td>
              <td>{row.target}</td>
              <th scope="row">
                {row.url ? <Link href={row.url}>{row.id}</Link> : row.id}
                {row.title ? <div className="ptb-sec__muted">{row.title}</div> : null}
              </th>
              <td>
                <code className="ptb-sec__break">{row.location}</code>
              </td>
              <td>{row.version ? <code>{row.version}</code> : null}</td>
              <td className={`ptb-sec__fix ptb-sec__fix--${row.fix.state.replace('/', '')}`}>
                {row.fix.label}
              </td>
            </tr>
          ))}
        </tbody>
      </Table>
      {f.omitted > 0 ? (
        <p className="ptb-sec__muted">
          {f.omitted} medium, low and unknown findings are not listed;{' '}
          <Link href={RESULTS_HREF}>results.json</Link> has all of them.
        </p>
      ) : null}
    </>
  );
}

function Accepted({rows}: {rows: Results['accepted']}): React.ReactElement {
  if (rows.length === 0) {
    return (
      <p className="ptb-sec__muted">
        No finding is accepted (<code>hack/security-accepted.txt</code> is empty).
      </p>
    );
  }
  return (
    <Table caption="Accepted findings, with how each was shown">
      <thead>
        <tr>
          <th scope="col">Scanner</th>
          <th scope="col">Finding</th>
          <th scope="col">Package</th>
          <th scope="col">Reason</th>
          <th scope="col">In this run</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((row) => (
          <tr key={`${row.scanner}/${row.id}/${row.package}`}>
            <td>{row.scanner}</td>
            <th scope="row">{row.id}</th>
            <td>
              <code>{row.package === '*' ? 'any package' : row.package}</code>
            </td>
            <td>{row.reason}</td>
            <td>{row.matched ? 'reported, marked accepted' : 'not reported'}</td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

function Report({r}: {r: Results}): React.ReactElement {
  return (
    <>
      {r.missing.length > 0 ? (
        <div className="ptb-sec__banner ptb-sec__banner--warn" role="note">
          <strong>{r.missing.length} scanner(s) did not produce a result:</strong>{' '}
          {r.missing.map((m) => m.name).join(', ')}. Their rows below read &ldquo;not run&rdquo; or
          &ldquo;failed&rdquo;, not zero.
        </div>
      ) : null}
      <p>
        Run of <code>{r.generated_at || 'an unknown time'}</code> on commit{' '}
        <Commit commit={r.commit} url={r.commit_url} />
        {r.run_url ? (
          <>
            {' '}
            (<Link href={r.run_url}>workflow run</Link>)
          </>
        ) : null}
        .
      </p>

      <h2 id="scanners">Scanners</h2>
      <Scanners rows={r.scanners} />

      <h2 id="findings">Open findings</h2>
      <Findings f={r.findings} />

      <h2 id="images">Images scanned</h2>
      <Images rows={r.images} />

      <h2 id="scorecard">OpenSSF Scorecard</h2>
      <Scorecard sc={r.scorecard} />

      <h2 id="accepted">Accepted findings</h2>
      <Accepted rows={r.accepted} />
    </>
  );
}

export default function Security(): React.ReactElement {
  const state = useResults(useBaseUrl('/security/results.json'));
  return (
    <Layout
      title="Security scan results"
      description="The results of the latest pgToolBox security run: every scanner, the open findings and whether a fix exists, the scanned image digests and the OpenSSF Scorecard score.">
      <main className="ptb-sec">
        <h1>Security scan results</h1>
        <p className="ptb-sec__lede">
          Generated from the latest security run on <code>main</code>. Every scanner is listed with what
          it did; a scanner that did not run says so and is never counted as zero findings.{' '}
          <Link href={RESULTS_HREF}>results.json</Link> has everything,{' '}
          <Link to="/docs/security/">the security model</Link> explains the design, and{' '}
          <Link href={POLICY_URL}>SECURITY.md</Link> says how to report a vulnerability.
        </p>
        <noscript>
          <div className="ptb-sec__banner">
            This page reads its data with JavaScript. The same results are in{' '}
            <a href="results.json">results.json</a>.
          </div>
        </noscript>

        {state.kind === 'loading' ? (
          <p className="ptb-sec__muted" role="status">
            Loading the latest results&hellip;
          </p>
        ) : null}
        {state.kind === 'absent' ? (
          <div className="ptb-sec__banner ptb-sec__banner--bad" role="status">
            <strong>No security run has been published yet.</strong> No scanner has produced a result, so
            nothing on this page says the project is free of findings.
          </div>
        ) : null}
        {state.kind === 'error' ? (
          <div className="ptb-sec__banner ptb-sec__banner--bad" role="alert">
            <strong>The results could not be shown.</strong> {state.reason}. Nothing on this page says the
            project is free of findings.
          </div>
        ) : null}
        {state.kind === 'ready' ? <Report r={state.results} /> : null}

        <h2 id="policy">How findings are handled</h2>
        <ul>
          <li>A fixable critical blocks a release and a pull request.</li>
          <li>
            A finding is accepted only when it is shown to be unreachable or not a vulnerability where it stands, and the acceptance records how.
          </li>
          <li>
            &ldquo;No fix available&rdquo; is never a reason to dismiss: such findings stay in the table
            above.
          </li>
        </ul>
        <p className="ptb-sec__muted">
          The full policy is in <Link href={POLICY_URL}>SECURITY.md</Link>.
        </p>
      </main>
    </Layout>
  );
}
