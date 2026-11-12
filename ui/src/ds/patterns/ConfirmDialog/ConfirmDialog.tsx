import { AlertDialog } from "@base-ui/react/alert-dialog";
import { type FormEvent, type ReactNode, useId, useState } from "react";
import { ApiError } from "../../../api/fetcher";
import { m } from "../../../paraglide/messages.js";

export type ConfirmDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The operation, as a verb phrase: "Restart billing". */
  title: string;
  /** What will happen, in plain words. */
  consequence: string;
  /** How long it usually takes, when known. */
  expectedDuration?: string;
  /** The word the reader must type: the Cluster's name. */
  confirmWord: string;
  /** The identity the action is performed with; the API server audit log records it (no Event). */
  actor: string;
  /** Sends the request. Resolves on 202; the dialog then closes and a tracker takes over. */
  onConfirm: () => Promise<void>;
  /** A choice the operation needs before it can be confirmed (promote: the instance). */
  children?: ReactNode;
  /** False while a required input in `children` is missing (a deny reason); the typed word alone is not enough. */
  canSubmit?: boolean;
  /** The submit button's word; "Request" by default. */
  submitLabel?: string;
  /** Where the action is recorded, when not the API server's audit log (a console action: the console's audit log). */
  recordedAs?: string;
  /**
   * Where focus goes when the dialog closes, asked at that moment. By default
   * it returns to the control that opened the dialog; when the outcome removes
   * that control (a disconnected card), return the element that now says what
   * happened. Undefined or null keeps the default.
   */
  finalFocus?: () => HTMLElement | null | undefined;
};

/**
 * ConfirmDialog — typed confirmation for a cluster operation.
 * Pessimistic: nothing is shown as done here; on 202 the dialog closes and an
 * OperationTracker follows the outcome. A refused request stays in the dialog
 * with its reason and request id.
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  consequence,
  expectedDuration,
  confirmWord,
  actor,
  onConfirm,
  children,
  canSubmit = true,
  submitLabel,
  recordedAs,
  finalFocus,
}: ConfirmDialogProps) {
  const [typed, setTyped] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ApiError | Error | null>(null);
  const inputId = useId();
  const matches = confirmWord !== "" && typed === confirmWord && canSubmit;

  const reset = (next: boolean) => {
    if (!next) {
      setTyped("");
      setError(null);
    }
    onOpenChange(next);
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!matches || pending) return;
    setPending(true);
    setError(null);
    try {
      await onConfirm();
      reset(false);
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)));
    } finally {
      setPending(false);
    }
  };

  return (
    <AlertDialog.Root open={open} onOpenChange={(next) => !pending && reset(next)}>
      <AlertDialog.Portal>
        <AlertDialog.Backdrop className="fixed inset-0 bg-overlay" />
        <AlertDialog.Viewport className="fixed inset-0 grid place-items-center p-4">
          <AlertDialog.Popup
            finalFocus={finalFocus ? () => finalFocus() ?? true : undefined}
            className="w-full max-w-lg rounded-dialog border border-line bg-surface p-5 text-fg shadow-overlay"
          >
            <form onSubmit={submit} className="flex flex-col gap-4">
              <AlertDialog.Title className="font-semibold text-heading">{title}</AlertDialog.Title>
              <AlertDialog.Description render={<div />} className="flex flex-col gap-2 text-body">
                <p>{consequence}</p>
                {expectedDuration && <p className="text-muted">{m.confirm_duration({ duration: expectedDuration })}</p>}
                <p className="text-muted">{recordedAs ?? m.confirm_recorded_as({ actor })}</p>
              </AlertDialog.Description>
              {children}
              <div className="flex flex-col gap-1">
                <label htmlFor={inputId} className="text-label">
                  {m.confirm_type_word()} <code className="font-code font-semibold">{confirmWord}</code>
                </label>
                <input
                  id={inputId}
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  autoComplete="off"
                  spellCheck={false}
                  className="h-9 rounded-control border border-line-strong bg-canvas px-2 font-code"
                />
              </div>
              {error && (
                <div role="alert" className="health rounded-control p-2 text-label" data-health="critical">
                  <p>{m.confirm_refused({ reason: error.message })}</p>
                  {error instanceof ApiError && error.problem?.detail && <p>{error.problem.detail}</p>}
                  {error instanceof ApiError && error.requestId && (
                    <p className="text-caption">{m.request_id({ id: error.requestId })}</p>
                  )}
                </div>
              )}
              <div className="flex justify-end gap-2">
                <AlertDialog.Close
                  disabled={pending}
                  className="h-9 rounded-control border border-line px-3 hover:bg-surface-2"
                >
                  {m.cancel()}
                </AlertDialog.Close>
                <button
                  type="submit"
                  disabled={!matches || pending}
                  aria-disabled={!matches || pending}
                  className="h-9 rounded-control border border-health-critical-line bg-health-critical-bg px-3 font-semibold text-health-critical-fg disabled:opacity-60"
                >
                  {pending ? m.confirm_sending() : (submitLabel ?? m.confirm_submit())}
                </button>
              </div>
            </form>
          </AlertDialog.Popup>
        </AlertDialog.Viewport>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}
