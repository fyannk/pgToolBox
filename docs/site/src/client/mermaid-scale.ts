// Sizes each Mermaid diagram between legible and fitting.
//
// Scaled to the column, a wide flowchart's labels shrink below reading size;
// drawn at its natural size, even a medium one overflows. A diagram fits the
// column as long as that keeps it at MIN_SCALE of its natural width or more
// (Mermaid's 16 px labels stay at about 11 px); past that it stops shrinking
// and scrolls sideways in its own box (custom.css).

import ExecutionEnvironment from '@docusaurus/ExecutionEnvironment';

const MIN_SCALE = 0.7;

function size(svg: SVGSVGElement): void {
  const natural = svg.viewBox.baseVal?.width;
  if (!natural) return;
  svg.style.width = '100%';
  svg.style.maxWidth = `${natural}px`;
  svg.style.minWidth = `${Math.round(natural * MIN_SCALE)}px`;
  svg.style.height = 'auto';
}

function sizeAll(root: ParentNode): void {
  root.querySelectorAll<SVGSVGElement>('.docusaurus-mermaid-container > svg').forEach(size);
}

if (ExecutionEnvironment.canUseDOM) {
  // Diagrams render after hydration and again on a theme switch.
  new MutationObserver(() => sizeAll(document)).observe(document.body, {childList: true, subtree: true});
  sizeAll(document);
}
