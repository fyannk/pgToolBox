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

// Diagrams wear fyannk/design: its palette, and IBM Plex Sans for labels.
// Mermaid sizes every box by measuring its label while it renders, so the
// font has to be one the site ships and has finished loading: with
// Mermaid's default (Trebuchet MS, a local font), the measure depends on
// the reader's fonts, and a browser that substitutes or restricts local
// fonts (Brave does) draws text wider than its box and crops it.
const FONT = '"IBM Plex Sans", system-ui, sans-serif';

// Mermaid derives shades from these, so they are colours, not var(--fy-*).
const PALETTE = {
  // node: the accent mixed 15% into the surface; edge: the accent 60% over
  // the page. The lichen green is the design's one accent, so the diagrams
  // carry it as a tint and never as a status colour.
  dark: {bg: '#040604', surface: '#090c0a', raised: '#0f1410', border: '#737b6b', text: '#e6e4d6', muted: '#9ca393', accent: '#b3bf3a', node: '#232711', edge: '#6d7524'},
  light: {bg: '#f2f1e8', surface: '#fbfaf3', raised: '#ffffff', border: '#b9bba8', text: '#14200f', muted: '#4f5a48', accent: '#4f6b0e', node: '#e6e9d8', edge: '#839653'},
};

function designed(config: {theme?: string}): object {
  const c = config.theme === 'dark' ? PALETTE.dark : PALETTE.light;
  return {
    ...config,
    theme: 'base',
    fontFamily: FONT,
    themeVariables: {
      fontFamily: FONT,
      fontSize: '15px',
      darkMode: config.theme === 'dark',
      background: c.bg,
      primaryColor: c.node,
      primaryTextColor: c.text,
      primaryBorderColor: c.edge,
      secondaryColor: c.raised,
      secondaryTextColor: c.text,
      secondaryBorderColor: c.border,
      tertiaryColor: c.surface,
      tertiaryTextColor: c.text,
      tertiaryBorderColor: c.border,
      lineColor: c.muted,
      textColor: c.text,
      edgeLabelBackground: c.bg,
      clusterBkg: c.surface,
      clusterBorder: c.border,
      titleColor: c.text,
      nodeTextColor: c.text,
      noteBkgColor: c.raised,
      noteTextColor: c.text,
      noteBorderColor: c.edge,
      actorBkg: c.node,
      actorBorder: c.edge,
      actorTextColor: c.text,
      actorLineColor: c.muted,
      signalColor: c.muted,
      signalTextColor: c.text,
      labelBoxBkgColor: c.node,
      labelBoxBorderColor: c.edge,
      labelTextColor: c.text,
      loopTextColor: c.text,
      activationBkgColor: c.node,
      activationBorderColor: c.edge,
      sequenceNumberColor: c.bg,
      stateLabelColor: c.text,
      stateBkg: c.node,
      altBackground: c.raised,
      transitionColor: c.muted,
      transitionLabelColor: c.text,
      specialStateColor: c.accent,
      innerEndBackground: c.accent,
    },
  };
}

// Mermaid keeps one global configuration, so renders must not interleave.
function render(text: string, config: object): Promise<RenderResult> {
  const job = queue.then(async () => {
    // Measured with a font that has not arrived, every label would be
    // sized for its fallback.
    await Promise.all(['400 15px "IBM Plex Sans"', '500 15px "IBM Plex Sans"'].map((f) => document.fonts.load(f)));
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
    render(value, designed(config)).then(
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
