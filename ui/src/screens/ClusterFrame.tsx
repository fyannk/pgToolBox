import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { useClusterSections } from "../app/sections";
import { ScrollStrip } from "../ds/patterns/ScrollStrip/ScrollStrip";
import { m } from "../paraglide/messages.js";

/** The frame of every cluster screen: breadcrumbs, the one <h1>, the screen navigation. */
export function ClusterFrame({
  namespace,
  cluster,
  children,
}: {
  namespace: string;
  cluster: string;
  children: ReactNode;
}) {
  const params = { namespace, cluster };
  const sections = useClusterSections();
  return (
    <>
      <nav aria-label={m.breadcrumbs()} className="text-label text-muted">
        <ol className="flex flex-wrap gap-1">
          <li>
            <Link to="/" className="text-accent hover:underline">
              {m.nav_clusters()}
            </Link>
            <span aria-hidden="true"> /</span>
          </li>
          <li>
            <Link to="/" search={{ q: namespace }} className="text-accent hover:underline">
              {namespace}
            </Link>
            <span aria-hidden="true"> /</span>
          </li>
          <li aria-current="page">{cluster}</li>
        </ol>
      </nav>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h1 className="break-all font-semibold text-display">{cluster}</h1>
        <Link
          to="/compare"
          search={{ left: `${namespace}/${cluster}` }}
          className="text-label text-accent hover:underline"
        >
          {m.compare_with()}
        </Link>
      </div>
      <nav aria-label={m.cluster_sections()} className="-mt-2 border-line border-b">
        <ScrollStrip>
          <ul className="flex gap-1">
            {sections.map((t) => (
              <li key={t.to}>
                <Link
                  to={t.to}
                  params={params}
                  activeOptions={{ exact: t.exact }}
                  className="-mb-px block whitespace-nowrap border-transparent border-b-2 px-3 py-2 font-code text-label text-muted hover:text-fg data-[status=active]:border-accent data-[status=active]:font-semibold data-[status=active]:text-fg"
                >
                  {t.label()}
                </Link>
              </li>
            ))}
          </ul>
        </ScrollStrip>
      </nav>
      {children}
    </>
  );
}
