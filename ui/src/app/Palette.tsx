import { useNavigate, useParams } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { useListClusters, useListInstances } from "../api/generated/endpoints/clusters/clusters";
import { CommandPalette, isPaletteShortcut, type PaletteGroup } from "../ds/patterns/CommandPalette/CommandPalette";
import { m } from "../paraglide/messages.js";
import { useClusterSections, useFeatures } from "./sections";

/**
 * The console's ⌘K palette: the clusters the reader may get, the pages of the
 * cluster on screen and its instances. Lists are read only while the palette
 * is open and come from the same queries the screens use (shared cache).
 */
export function Palette() {
  const [open, setOpen] = useState(false);
  const navigate = useNavigate();
  const { namespace, cluster } = useParams({ strict: false });
  const inCluster = Boolean(namespace && cluster);
  const sections = useClusterSections();
  const features = useFeatures();

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!isPaletteShortcut(e)) return;
      e.preventDefault();
      setOpen((o) => !o);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const clusters = useListClusters({ query: { enabled: open } });
  const instances = useListInstances(namespace ?? "", cluster ?? "", { query: { enabled: open && inCluster } });

  const groups: PaletteGroup[] = [];
  if (namespace && cluster) {
    const params = { namespace, cluster };
    groups.push({
      id: "pages",
      label: m.palette_group_pages({ cluster }),
      items: sections.map((s) => ({
        id: `page:${s.to}`,
        label: s.label(),
        hint: `${namespace}/${cluster}`,
        onSelect: () => void navigate({ to: s.to, params }),
      })),
    });
    groups.push({
      id: "instances",
      label: m.palette_group_instances({ cluster }),
      loading: instances.isPending,
      items: (instances.data?.items ?? []).map((i) => ({
        id: `pod:${i.name}`,
        label: i.name,
        hint: `${namespace}/${cluster}`,
        health: i.health,
        onSelect: () =>
          void navigate({ to: "/c/$namespace/$cluster/instances/$pod", params: { ...params, pod: i.name } }),
      })),
    });
  }
  groups.push({
    id: "clusters",
    label: m.palette_group_clusters(),
    loading: clusters.isPending,
    items: (clusters.data?.items ?? []).map((c) => ({
      id: `cluster:${c.ref.namespace}/${c.ref.name}`,
      label: c.ref.name,
      hint: c.ref.namespace,
      health: c.verdict.health,
      onSelect: () =>
        void navigate({ to: "/c/$namespace/$cluster", params: { namespace: c.ref.namespace, cluster: c.ref.name } }),
    })),
  });
  groups.push({
    id: "console",
    label: m.palette_group_console(),
    items: [
      { id: "console:clusters", label: m.nav_clusters(), onSelect: () => void navigate({ to: "/" }) },
      { id: "console:admin", label: m.nav_admin(), onSelect: () => void navigate({ to: "/admin" }) },
      { id: "console:access", label: m.nav_access(), onSelect: () => void navigate({ to: "/access" }) },
      { id: "console:insight", label: m.nav_fleet_insight(), onSelect: () => void navigate({ to: "/insight" }) },
      { id: "console:compare", label: m.nav_compare(), onSelect: () => void navigate({ to: "/compare" }) },
      ...(features?.mcp
        ? [
            {
              id: "console:assistants",
              label: m.palette_connect_assistant(),
              hint: m.nav_assistants(),
              onSelect: () => void navigate({ to: "/assistants" }),
            },
          ]
        : []),
      ...(features?.digest
        ? [{ id: "console:digests", label: m.nav_digests(), onSelect: () => void navigate({ to: "/digests" }) }]
        : []),
    ],
  });

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-keyshortcuts="Meta+K Control+K"
        className="flex h-8 items-center gap-2 rounded-control border border-topbar-line px-2 text-label text-topbar-fg hover:underline"
      >
        {m.palette_open()}
        <kbd className="font-code text-caption text-topbar-muted">⌘K</kbd>
      </button>
      <CommandPalette open={open} onOpenChange={setOpen} groups={groups} />
    </>
  );
}
