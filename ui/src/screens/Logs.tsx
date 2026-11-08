import { Link } from "@tanstack/react-router";
import { useId } from "react";
import { useGetCluster } from "../api/generated/endpoints/clusters/clusters";
import { useGetLogTail } from "../api/generated/endpoints/logs/logs";
import type { LogTail } from "../api/generated/model";
import { logStreamPath, MAX_LOG_LINES, useLogStream } from "../api/logStream";
import { useFeatures } from "../app/sections";
import { LogViewer } from "../ds/patterns/LogViewer/LogViewer";
import { formatUtc } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { QueryState } from "./QueryState";

type Props = {
  namespace: string;
  cluster: string;
  pod: string;
  container?: string;
  previous?: boolean;
  onChange: (next: { container?: string; previous?: boolean }) => void;
};

function Controls({ tail, previous, onChange }: { tail: LogTail; previous: boolean; onChange: Props["onChange"] }) {
  const selectId = useId();
  const checkId = useId();
  const containers = tail.containers ?? [tail.container];
  return (
    <div className="flex flex-wrap items-center gap-4 text-label">
      <span className="flex items-center gap-2">
        <label htmlFor={selectId}>{m.logs_container()}</label>
        <select
          id={selectId}
          value={tail.container}
          onChange={(e) => onChange({ container: e.target.value, previous })}
          className="h-8 rounded-control border border-line-strong bg-canvas px-1"
        >
          {containers.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
      </span>
      <span className="flex items-center gap-2">
        <input
          id={checkId}
          type="checkbox"
          checked={previous}
          onChange={(e) => onChange({ container: tail.container, previous: e.target.checked })}
        />
        <label htmlFor={checkId}>{m.logs_previous()}</label>
      </span>
    </div>
  );
}

function Live({
  namespace,
  cluster,
  pod,
  container,
}: {
  namespace: string;
  cluster: string;
  pod: string;
  container: string;
}) {
  const stream = useLogStream(logStreamPath(namespace, cluster, pod, container));
  return (
    <LogViewer
      title={m.log_label({ pod: `${pod} (${container})` })}
      lines={stream.lines}
      state={stream.state}
      dropped={stream.dropped}
      endReason={stream.endReason}
      maxLines={MAX_LOG_LINES}
    />
  );
}

/**
 * One instance's log: the server checks you may get pods/log (the tail read),
 * then streams it live; the previous container's log is a fixed read.
 */
export function LogsScreen({ namespace, cluster, pod, container, previous = false, onChange }: Props) {
  const features = useFeatures();
  const overview = useGetCluster(namespace, cluster);
  const off = features?.logs === false;
  const denied = overview.data?.capabilities.logs === false && !overview.data.capabilities.undecided?.includes("logs");
  // Asked only when the console serves logs and the API server did not already say no.
  const tail = useGetLogTail(
    namespace,
    cluster,
    pod,
    { container, previous: previous || undefined },
    { query: { enabled: !off && !denied } },
  );
  return (
    <div className="flex flex-col gap-3">
      <Link
        to="/c/$namespace/$cluster/instances/$pod"
        params={{ namespace, cluster, pod }}
        className="text-label text-accent hover:underline"
      >
        ← {m.logs_back({ pod })}
      </Link>
      <h2 className="font-semibold text-title">{m.logs_of({ pod })}</h2>
      {off ? (
        <p className="health rounded-panel p-3 text-label" data-health="inactive">
          {m.logs_feature_off()}
        </p>
      ) : denied ? (
        <p role="alert" className="health rounded-panel p-3 text-label" data-health="inactive">
          {m.logs_not_allowed_detail()}
        </p>
      ) : (
        <QueryState query={tail} what={m.logs_of({ pod })}>
          {(data) => (
            <>
              <Controls tail={data} previous={previous} onChange={onChange} />
              {data.truncated && <p className="text-caption text-muted">{m.logs_truncated()}</p>}
              {previous ? (
                <>
                  <p className="text-caption text-muted">{m.logs_previous_note({ time: formatUtc(data.readAt) })}</p>
                  <LogViewer
                    title={m.log_label({ pod: `${pod} (${data.container})` })}
                    lines={(data.lines ?? []).map((l, i) => ({ seq: i, time: l.at, text: l.text }))}
                    state="ended"
                  />
                </>
              ) : (
                <Live namespace={namespace} cluster={cluster} pod={pod} container={data.container} />
              )}
              <p className="text-caption text-muted">{m.logs_redacted()}</p>
            </>
          )}
        </QueryState>
      )}
    </div>
  );
}
