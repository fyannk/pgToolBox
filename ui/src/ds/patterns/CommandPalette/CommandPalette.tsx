import { Autocomplete } from "@base-ui/react/autocomplete";
import { Dialog } from "@base-ui/react/dialog";
import { useId } from "react";
import { m } from "../../../paraglide/messages.js";
import { StatusChip } from "../StatusChip/StatusChip";

export type PaletteItem = {
  /** Unique across the palette. */
  id: string;
  /** What the reader looks for: a cluster, a page, a pod. */
  label: string;
  /** A second line: the namespace, the section, the role. */
  hint?: string;
  /** The Go health of the target, when it has one (clusters, instances). */
  health?: string;
  onSelect: () => void;
};
export type PaletteGroup = { id: string; label: string; items: PaletteItem[]; loading?: boolean };

const matchText = (item: PaletteItem) => `${item.label} ${item.hint ?? ""}`;

/**
 * CommandPalette (⌘K / Ctrl+K) — jump to a cluster, a page of the current
 * cluster or one of its instances. A Base UI Autocomplete inside
 * a Dialog: the input keeps focus, arrows move the highlight, Enter opens,
 * Escape closes and focus returns to where it was. It filters and navigates;
 * every state it shows comes from the Go view models already read.
 */
export function CommandPalette({
  open,
  onOpenChange,
  groups,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  groups: PaletteGroup[];
}) {
  const hintId = useId();
  const shown = groups.filter((g) => g.items.length > 0);
  const loading = groups.filter((g) => g.loading && g.items.length === 0);
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Backdrop className="fixed inset-0 bg-overlay" />
        <Dialog.Viewport className="fixed inset-0 flex items-start justify-center px-2 pt-16 pb-2">
          <Dialog.Popup
            aria-label={m.palette_label()}
            className="flex max-h-[80dvh] w-full max-w-xl flex-col overflow-hidden rounded-dialog border border-line bg-surface text-fg shadow-overlay"
          >
            <Autocomplete.Root
              open
              inline
              items={shown}
              itemToStringValue={matchText}
              autoHighlight="always"
              keepHighlight
            >
              <Autocomplete.Input
                aria-label={m.palette_search()}
                aria-describedby={hintId}
                placeholder={m.palette_placeholder()}
                className="h-11 w-full border-line border-b bg-surface px-3 text-body outline-none placeholder:text-muted"
              />
              <Dialog.Close className="sr-only">{m.palette_close()}</Dialog.Close>
              <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain">
                <Autocomplete.Empty>
                  <p className="px-3 py-6 text-label text-muted">{m.palette_empty()}</p>
                </Autocomplete.Empty>
                <Autocomplete.List className="py-1">
                  {(group: PaletteGroup) => (
                    <Autocomplete.Group key={group.id} items={group.items} className="pb-1">
                      <Autocomplete.GroupLabel className="px-3 pt-2 pb-1 font-semibold text-caption text-muted">
                        {group.label}
                      </Autocomplete.GroupLabel>
                      <Autocomplete.Collection>
                        {(item: PaletteItem) => (
                          <Autocomplete.Item
                            key={item.id}
                            value={item}
                            onClick={() => {
                              onOpenChange(false);
                              item.onSelect();
                            }}
                            className="flex min-h-row cursor-default items-center justify-between gap-3 px-3 py-1 text-label outline-none data-highlighted:bg-accent-weak"
                          >
                            <span className="flex min-w-0 flex-col">
                              <span className="truncate font-medium">{item.label}</span>
                              {item.hint && <span className="truncate text-caption text-muted">{item.hint}</span>}
                            </span>
                            {item.health && <StatusChip health={item.health} size="sm" />}
                          </Autocomplete.Item>
                        )}
                      </Autocomplete.Collection>
                    </Autocomplete.Group>
                  )}
                </Autocomplete.List>
                {loading.map((g) => (
                  <p key={g.id} aria-live="polite" className="px-3 py-1 text-caption text-muted">
                    {m.palette_loading({ what: g.label })}
                  </p>
                ))}
              </div>
              <p id={hintId} className="border-line border-t px-3 py-2 text-caption text-muted">
                {m.palette_hint()}
              </p>
            </Autocomplete.Root>
          </Dialog.Popup>
        </Dialog.Viewport>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** True for the palette's shortcut: ⌘K on macOS, Ctrl+K elsewhere. */
export function isPaletteShortcut(e: KeyboardEvent): boolean {
  return (e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === "k";
}
