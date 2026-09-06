"use client";

import { useSyncExternalStore } from "react";

function subscribe(onChange: () => void): () => void {
  document.addEventListener("visibilitychange", onChange);
  return () => document.removeEventListener("visibilitychange", onChange);
}

function getSnapshot(): boolean {
  return !document.hidden;
}

function getServerSnapshot(): boolean {
  return true;
}

/** True while the tab is visible. Live updates and polling pause when it is not. */
export function useDocumentVisible(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}
