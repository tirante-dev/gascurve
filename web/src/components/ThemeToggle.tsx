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

export function applyTheme(choice: ThemeChoice): void {
  const root = document.documentElement;
  if (choice === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);
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
  window.addEventListener("storage", onChange);
  return () => {
    listeners.delete(onChange);
    window.removeEventListener("storage", onChange);
  };
}

function getServerSnapshot(): ThemeChoice {
  return "system";
}

/** Cycles system, light, dark. The layout script applies the stored choice before paint. */
export function ThemeToggle() {
  const choice = useSyncExternalStore(subscribe, readTheme, getServerSnapshot);
  const next = ORDER[(ORDER.indexOf(choice) + 1) % ORDER.length];
  return (
    <button
      type="button"
      className="rounded-md border border-hairline bg-surface px-2.5 py-1 text-xs font-medium text-ink-2 hover:text-ink"
      onClick={() => applyTheme(next)}
      aria-label={`Theme: ${choice}. Switch to ${next}`}
      title={`Switch to ${next} theme`}
    >
      theme: {choice}
    </button>
  );
}
