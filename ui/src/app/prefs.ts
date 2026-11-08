// Per-reader preferences (optimistic, local): theme and density.
// Applied as attributes on <html>, which tokens.css and styles.css key on.
// Applied from the module entry, not an inline script (CSP), so a reader who
// pinned a theme against the OS preference may see one frame of the other.

export type ThemePref = "system" | "light" | "dark";
export type DensityPref = "comfortable" | "compact";

const THEME = "pgtoolbox-console.theme";
const DENSITY = "pgtoolbox-console.density";

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}
function write(key: string, value: string | null) {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    // Preference lasts for the page only.
  }
}

export function getTheme(): ThemePref {
  const v = read(THEME);
  return v === "light" || v === "dark" ? v : "system";
}
export function getDensity(): DensityPref {
  return read(DENSITY) === "compact" ? "compact" : "comfortable";
}

export function applyTheme(t: ThemePref, root = document.documentElement) {
  if (t === "system") delete root.dataset.theme;
  else root.dataset.theme = t;
}
export function applyDensity(d: DensityPref, root = document.documentElement) {
  if (d === "compact") root.dataset.density = "compact";
  else delete root.dataset.density;
}

export function setTheme(t: ThemePref) {
  write(THEME, t === "system" ? null : t);
  applyTheme(t);
}
export function setDensity(d: DensityPref) {
  write(DENSITY, d === "compact" ? d : null);
  applyDensity(d);
}

export function applyStoredPrefs() {
  applyTheme(getTheme());
  applyDensity(getDensity());
}

// The wiring drawing: which frames the reader keeps folded.
const FOLDED = "pgtoolbox-console.topology.folded";

export function getFolded(): string[] {
  return (read(FOLDED) ?? "").split(",").filter(Boolean);
}
export function setFolded(kinds: string[]) {
  write(FOLDED, kinds.length > 0 ? kinds.join(",") : null);
}
