"use client";

import { useSyncExternalStore } from "react";

export type ThemeChoice = "system" | "light" | "dark";
export const THEME_STORAGE_KEY = "gascurve:theme";
const ORDER: ThemeChoice[] = ["system", "light", "dark"];
const listeners = new Set<() => void>();

export function readTheme(): ThemeChoice {
  try {
    const value = window.localStorage.getItem(THEME_STORAGE_KEY);
    return value === "light" || value === "dark" ? value : "system";
  } catch {
    return "system";
  }
}

/** Stamps `choice` on the root element. The pre-paint script in the layout does the same thing for the first render. */
export function stampTheme(choice: ThemeChoice): void {
  const root = document.documentElement;
  if (choice === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);
}

/** Applies whatever is stored now, without writing it back: what another tab's change means for this document. */
export function syncTheme(): void {
  stampTheme(readTheme());
}

export function applyTheme(choice: ThemeChoice): void {
  stampTheme(choice);
  try {
    if (choice === "system") window.localStorage.removeItem(THEME_STORAGE_KEY);
    else window.localStorage.setItem(THEME_STORAGE_KEY, choice);
  } catch {
    // Storage may be blocked; the attribute still applies for this page view.
  }
  listeners.forEach((listener) => listener());
}

function subscribe(onChange: () => void): () => void {
  listeners.add(onChange);
  // Another tab's change reaches this document as a storage event: the page
  // has to take the new choice on before the control reports it, or the
  // button would name a theme the page is not wearing.
  const onStorage = (event: StorageEvent) => {
    if (event.key !== null && event.key !== THEME_STORAGE_KEY) return;
    syncTheme();
    onChange();
  };
  window.addEventListener("storage", onStorage);
  return () => {
    listeners.delete(onChange);
    window.removeEventListener("storage", onStorage);
  };
}

function getServerSnapshot(): ThemeChoice {
  return "system";
}

const GLYPH: Record<ThemeChoice, string> = { system: "◐", light: "○", dark: "●" };

/** Cycles system, light, dark. The layout script applies the stored choice before paint. */
export function ThemeToggle() {
  const choice = useSyncExternalStore(subscribe, readTheme, getServerSnapshot);
  const next = ORDER[(ORDER.indexOf(choice) + 1) % ORDER.length];
  return (
    <button
      type="button"
      className="vw-control inline-flex items-center gap-1.5 whitespace-nowrap px-3 py-1 text-xs font-medium"
      onClick={() => applyTheme(next)}
      aria-label={`Theme: ${choice}. Switch to ${next}`}
      title={`Switch to ${next} theme`}
    >
      <span className="text-accent-2-text" aria-hidden="true">
        {GLYPH[choice]}
      </span>
      theme: {choice}
    </button>
  );
}
