// Ejected from @docusaurus/theme-mermaid 3.10.2 to fix blank diagrams in dark
// mode.
//
// The upstream hook renders each diagram once at hydration (light) and again
// when the colour mode resolves to dark, both with the same element id, on
// Mermaid's single mutable instance. Two renders sharing an id remove each
// other's scratch nodes, and the stale result is never discarded: on a page
// opened in dark mode, every diagram after the first came out empty. Here
// renders run one at a time, each with its own id, and a render whose inputs
// changed before it finished is dropped.

import React, {useEffect, useRef, useState, type ReactNode} from 'react';
import ErrorBoundary from '@docusaurus/ErrorBoundary';
import {ErrorBoundaryErrorMessageFallback} from '@docusaurus/theme-common';
import {MermaidContainerClassName, useMermaidConfig} from '@docusaurus/theme-mermaid/client';
import type {RenderResult} from 'mermaid';
import styles from './styles.module.css';

let queue: Promise<unknown> = Promise.resolve();
let sequence = 0;

// Mermaid keeps one global configuration, so renders must not interleave.
function render(text: string, config: object): Promise<RenderResult> {
  const job = queue.then(async () => {
    const {default: mermaid} = await import('mermaid');
    const id = `mermaid-svg-${++sequence}`;
    mermaid.initialize(config);
    try {
      return await mermaid.render(id, text);
    } catch (e) {
      // On error Mermaid leaves its error drawing in the document.
      document.querySelector(`#d${id}`)?.remove();
      throw e;
    }
  });
  queue = job.catch(() => undefined);
  return job;
}

function MermaidRenderResult({renderResult}: {renderResult: RenderResult}): ReactNode {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (ref.current) renderResult.bindFunctions?.(ref.current);
  }, [renderResult]);
  return (
    <div
      ref={ref}
      className={`${MermaidContainerClassName} ${styles.container}`}
      // eslint-disable-next-line react/no-danger
      dangerouslySetInnerHTML={{__html: renderResult.svg}}
    />
  );
}

function MermaidRenderer({value}: {value: string}): ReactNode {
  const config = useMermaidConfig();
  const [result, setResult] = useState<RenderResult | null>(null);
  useEffect(() => {
    let current = true;
    render(value, config).then(
      (r) => current && setResult(r),
      (e) =>
        current &&
        setResult(() => {
          // Rethrown during render, so the error boundary shows it.
          throw e;
        }),
    );
    return () => {
      current = false;
    };
  }, [value, config]);
  return result === null ? null : <MermaidRenderResult renderResult={result} />;
}

export default function Mermaid(props: {value: string}): ReactNode {
  return (
    <ErrorBoundary fallback={(params) => <ErrorBoundaryErrorMessageFallback {...params} />}>
      <MermaidRenderer {...props} />
    </ErrorBoundary>
  );
}
