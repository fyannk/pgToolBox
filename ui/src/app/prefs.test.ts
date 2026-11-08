import { afterEach, expect, test } from "vitest";
import { applyStoredPrefs, getDensity, getTheme, setDensity, setTheme } from "./prefs";

afterEach(() => {
  setTheme("system");
  setDensity("comfortable");
});

test("theme and density are attributes on <html>, persisted per reader", () => {
  setTheme("dark");
  setDensity("compact");
  expect(document.documentElement.dataset.theme).toBe("dark");
  expect(document.documentElement.dataset.density).toBe("compact");
  delete document.documentElement.dataset.theme;
  applyStoredPrefs();
  expect(document.documentElement.dataset.theme).toBe("dark");
  expect(getTheme()).toBe("dark");
  expect(getDensity()).toBe("compact");
});

test("system removes the override so the OS preference applies", () => {
  setTheme("light");
  setTheme("system");
  expect(document.documentElement.dataset.theme).toBeUndefined();
});

test("compact rows are shorter than comfortable ones", () => {
  const probe = document.createElement("div");
  probe.className = "h-row";
  document.body.append(probe);
  setDensity("comfortable");
  const comfortable = probe.getBoundingClientRect().height;
  setDensity("compact");
  const compactH = probe.getBoundingClientRect().height;
  probe.remove();
  expect(comfortable).toBe(36);
  expect(compactH).toBe(28);
});
