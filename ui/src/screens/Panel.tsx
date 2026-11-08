import { type ReactNode, useId } from "react";

/** A titled section of a screen (one <h2>). */
export function Panel({ title, children, actions }: { title: string; children: ReactNode; actions?: ReactNode }) {
  const id = useId();
  return (
    <section aria-labelledby={id} className="flex flex-col gap-2 rounded-panel border border-line bg-surface p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 id={id} className="font-semibold text-title">
          {title}
        </h2>
        {actions}
      </div>
      {children}
    </section>
  );
}

/** Label/value pairs: <Facts><Fact label="…">value</Fact></Facts>. */
export function Facts({ children }: { children: ReactNode }) {
  return <dl className="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1 text-label">{children}</dl>;
}

export function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-muted">{label}</dt>
      <dd className="m-0 min-w-0 break-words">{children}</dd>
    </div>
  );
}

/** A table with a visible title: on screens with several tables the caption alone is not enough to scan. */
export function Titled({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-2">
      <h2 className="font-semibold text-title">{title}</h2>
      {children}
    </div>
  );
}
