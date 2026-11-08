import { Tabs as BaseTabs } from "@base-ui/react/tabs";
import type { ReactNode } from "react";

export type TabItem = { value: string; label: ReactNode; panel: ReactNode };

/**
 * Tabs — in-page tabs with a roving tabindex: one tab stop, Arrow keys move,
 * Home/End jump (Base UI). Route navigation uses NavTabs links instead.
 */
export function Tabs({
  label,
  items,
  value,
  onValueChange,
}: {
  label: string;
  items: TabItem[];
  value?: string;
  onValueChange?: (value: string) => void;
}) {
  return (
    <BaseTabs.Root
      defaultValue={value === undefined ? items[0]?.value : undefined}
      value={value}
      onValueChange={(v) => onValueChange?.(String(v))}
    >
      <BaseTabs.List aria-label={label} className="flex gap-1 border-line border-b">
        {items.map((t) => (
          <BaseTabs.Tab
            key={t.value}
            value={t.value}
            className="-mb-px border-transparent border-b-2 px-3 py-2 font-code text-label text-muted hover:text-fg data-[active]:border-accent data-[active]:font-semibold data-[active]:text-fg"
          >
            {t.label}
          </BaseTabs.Tab>
        ))}
      </BaseTabs.List>
      {items.map((t) => (
        <BaseTabs.Panel key={t.value} value={t.value} className="pt-4">
          {t.panel}
        </BaseTabs.Panel>
      ))}
    </BaseTabs.Root>
  );
}
