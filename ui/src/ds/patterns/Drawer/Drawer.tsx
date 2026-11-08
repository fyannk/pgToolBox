import { Drawer as BaseDrawer } from "@base-ui/react/drawer";
import type { ReactNode, RefObject } from "react";
import { m } from "../../../paraglide/messages.js";

/**
 * Drawer — a side panel for a detail that should not lose the list behind it
 * (an instance from the instances table). Base UI traps focus, closes on
 * Escape and returns focus to the opener.
 */
export function Drawer({
  open,
  onOpenChange,
  title,
  description,
  children,
  returnFocus,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  children: ReactNode;
  /** Where focus goes on close when the drawer is opened by state rather than a Trigger. */
  returnFocus?: RefObject<HTMLElement | null>;
}) {
  return (
    <BaseDrawer.Root open={open} onOpenChange={onOpenChange} swipeDirection="right">
      <BaseDrawer.Portal>
        <BaseDrawer.Backdrop className="fixed inset-0 bg-overlay" />
        <BaseDrawer.Viewport className="fixed inset-0 flex justify-end">
          <BaseDrawer.Popup
            finalFocus={returnFocus ?? true}
            className="flex h-full w-full max-w-xl flex-col overflow-y-auto border-line border-l bg-surface text-fg shadow-overlay"
          >
            <BaseDrawer.Content className="flex flex-col gap-4 p-5">
              <div className="flex items-start justify-between gap-4">
                <div>
                  <BaseDrawer.Title className="font-semibold text-heading">{title}</BaseDrawer.Title>
                  {description && (
                    <BaseDrawer.Description className="text-label text-muted">{description}</BaseDrawer.Description>
                  )}
                </div>
                <BaseDrawer.Close className="rounded-control border border-line px-2 py-1 text-label hover:bg-surface-2">
                  {m.close()}
                </BaseDrawer.Close>
              </div>
              {children}
            </BaseDrawer.Content>
          </BaseDrawer.Popup>
        </BaseDrawer.Viewport>
      </BaseDrawer.Portal>
    </BaseDrawer.Root>
  );
}
