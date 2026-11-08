// Theme × density variants of a story (every pattern has stories
// for each theme and density). Spread a base story into the variant.
type Globals = Record<string, unknown>;
const withGlobals = <S extends object>(s: S, extra: Globals): S => ({
  ...s,
  globals: { ...((s as { globals?: Globals }).globals ?? {}), ...extra },
});

export const dark = <S extends object>(s: S): S => withGlobals(s, { theme: "dark" });
export const compact = <S extends object>(s: S): S => withGlobals(s, { density: "compact" });
export const darkCompact = <S extends object>(s: S): S => withGlobals(s, { theme: "dark", density: "compact" });
export const french = <S extends object>(s: S): S => withGlobals(s, { locale: "fr" });
