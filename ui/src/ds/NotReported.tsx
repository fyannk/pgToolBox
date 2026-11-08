import type { ReactNode } from "react";
import { m } from "../paraglide/messages.js";

/**
 * An absent JSON field means "not reported" (viewmodel/vocabulary.go), never
 * zero or false. Render the value when present, the words otherwise.
 */
export function Reported<T>({ value, children }: { value: T | null | undefined; children?: (v: T) => ReactNode }) {
  if (value === null || value === undefined || value === "") {
    return <span className="text-muted italic">{m.not_reported()}</span>;
  }
  return <>{children ? children(value) : String(value)}</>;
}
