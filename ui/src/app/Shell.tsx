import { Link, Outlet, useLocation } from "@tanstack/react-router";
import { useEffect, useId, useState } from "react";
import { DEFAULT_LOGIN_URL, onReauth } from "../api/fetcher";
import { backstopInterval, useLive } from "../api/live";
import { type Session, useSession } from "../api/session";
import consoleMark from "../ds/brand/console-mark.svg";
import { LiveBanner } from "../ds/patterns/LiveBanner/LiveBanner";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { getLocale, locales, setLocale } from "../paraglide/runtime.js";
import { Palette } from "./Palette";
import { type DensityPref, getDensity, getTheme, setDensity, setTheme, type ThemePref } from "./prefs";
import { useFeatures } from "./sections";
import { TrackedOperations } from "./TrackedOperations";

function useSignInHref(loginUrl: string) {
  const { href } = useLocation();
  return `${loginUrl}${loginUrl.includes("?") ? "&" : "?"}rd=${encodeURIComponent(href)}`;
}

/** The session ended (a 401): what is shown is no longer refreshed. */
function ReauthBanner({ loginUrl }: { loginUrl: string }) {
  const target = useSignInHref(loginUrl);
  return (
    <div role="alert" className="health border-x-0 border-t-0 px-4 py-2 text-label" data-health="warning">
      {m.reauth_banner()}{" "}
      <a href={target} className="font-semibold underline">
        {m.reauth_action()}
      </a>
    </div>
  );
}

/** The stream said the session ends soon and cannot renew itself (reauth, refreshable false). */
function ReauthSoon({ expiresAt }: { expiresAt: string }) {
  const target = useSignInHref(DEFAULT_LOGIN_URL);
  return (
    <div role="status" className="health border-x-0 border-t-0 px-4 py-2 text-label" data-health="info">
      {m.reauth_soon()} <Time iso={expiresAt} />{" "}
      <a href={target} className="font-semibold underline">
        {m.reauth_action()}
      </a>
    </div>
  );
}

/** Who is signed in, and the sign-out form (POST, same origin: form-action 'self'). */
export function SessionMenuView({ session }: { session: Session }) {
  return (
    <form method="post" action={session.logoutURL} className="flex items-center gap-2 text-label text-topbar-muted">
      <span>{m.session_signed_in({ user: session.username })}</span>
      {/* Offered only where the console serves MCP; a direct link elsewhere gets the page's honest 404 state. */}
      {session.features.mcp && (
        <Link to="/assistants" className="text-topbar-fg hover:underline">
          {m.nav_assistants()}
        </Link>
      )}
      <button
        type="submit"
        className="rounded-control border border-topbar-line px-2 py-0.5 text-topbar-fg hover:underline"
      >
        {m.session_sign_out()}
      </button>
    </form>
  );
}

function SessionMenu() {
  const session = useSession();
  return session.data ? <SessionMenuView session={session.data} /> : null;
}

function Preferences() {
  const [theme, setThemeState] = useState<ThemePref>(getTheme);
  const [density, setDensityState] = useState<DensityPref>(getDensity);
  const themeId = useId();
  const densityId = useId();
  const langId = useId();
  const select = "h-8 rounded-control border border-topbar-line bg-topbar px-1 text-label text-topbar-fg";
  return (
    <div className="flex flex-wrap items-center gap-2 text-label text-topbar-muted">
      <label htmlFor={themeId}>{m.pref_theme()}</label>
      <select
        id={themeId}
        className={select}
        value={theme}
        onChange={(e) => {
          const v = e.target.value as ThemePref;
          setTheme(v);
          setThemeState(v);
        }}
      >
        <option value="system">{m.pref_theme_system()}</option>
        <option value="light">{m.pref_theme_light()}</option>
        <option value="dark">{m.pref_theme_dark()}</option>
      </select>
      <label htmlFor={densityId}>{m.pref_density()}</label>
      <select
        id={densityId}
        className={select}
        value={density}
        onChange={(e) => {
          const v = e.target.value as DensityPref;
          setDensity(v);
          setDensityState(v);
        }}
      >
        <option value="comfortable">{m.pref_density_comfortable()}</option>
        <option value="compact">{m.pref_density_compact()}</option>
      </select>
      <label htmlFor={langId}>{m.pref_language()}</label>
      <select
        id={langId}
        className={select}
        value={getLocale()}
        onChange={(e) => setLocale(e.target.value as (typeof locales)[number])}
      >
        {locales.map((l) => (
          <option key={l} value={l}>
            {l === "fr" ? "Français" : "English"}
          </option>
        ))}
      </select>
    </div>
  );
}

/** The application frame: skip link, top bar, live and re-authentication banners, main landmark. */
export function Shell() {
  const live = useLive();
  const features = useFeatures();
  const [loginUrl, setLoginUrl] = useState<string | null>(null);
  useEffect(() => onReauth(setLoginUrl), []);
  return (
    <div className="min-h-dvh bg-canvas text-fg">
      <a
        href="#main"
        className="sr-only rounded-control bg-surface px-3 py-2 focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50"
      >
        {m.skip_to_content()}
      </a>
      <header className="flex flex-wrap items-center justify-between gap-3 border-topbar-line border-b bg-topbar px-4 py-2 text-topbar-fg">
        <nav aria-label={m.nav_console()} className="flex flex-wrap items-center gap-4">
          {/* The fyannk/design product wordmark: "pg" muted, the name in the
              text colour, Plex Mono 600, the accent block cursor. A product
              name, not a translatable string. */}
          <Link to="/" className="flex items-center gap-2 font-code font-semibold text-title tracking-tight">
            <img src={consoleMark} alt="" width={24} height={24} className="size-6" />
            <span className="text-topbar-muted">pg</span>
            <span className="-ml-2">ToolBox</span>
            <span aria-hidden="true" className="-ml-2 text-accent">
              ▍
            </span>
          </Link>
          <Link to="/insight" className="font-code text-label text-topbar-fg hover:text-accent">
            {m.nav_fleet_insight()}
          </Link>
          <Link to="/access" className="font-code text-label text-topbar-fg hover:text-accent">
            {m.nav_access()}
          </Link>
          <Link to="/compare" className="font-code text-label text-topbar-fg hover:text-accent">
            {m.nav_compare()}
          </Link>
          {features?.digest && (
            <Link to="/digests" className="font-code text-label text-topbar-fg hover:text-accent">
              {m.nav_digests()}
            </Link>
          )}
        </nav>
        <div className="flex flex-wrap items-center gap-4">
          <Palette />
          <Preferences />
          <SessionMenu />
        </div>
      </header>
      {loginUrl && <ReauthBanner loginUrl={loginUrl} />}
      {!loginUrl && live.reauth && !live.reauth.refreshable && <ReauthSoon expiresAt={live.reauth.expiresAt} />}
      <LiveBanner connected={live.status !== "down"} intervalMs={backstopInterval(live)} />
      <TrackedOperations />
      <main id="main" tabIndex={-1} className="mx-auto flex max-w-measure flex-col gap-4 p-4 outline-none">
        <Outlet />
      </main>
    </div>
  );
}
