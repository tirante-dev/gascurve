import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

// Node 22+ exposes a `localStorage` getter on globalThis that yields undefined
// unless the process runs with --localstorage-file, and the jsdom environment
// does not override keys that already exist. Provide an in-memory Storage so
// hooks that persist choices behave as they do in a browser.
class MemoryStorage implements Storage {
  private map = new Map<string, string>();
  get length(): number {
    return this.map.size;
  }
  clear(): void {
    this.map.clear();
  }
  getItem(key: string): string | null {
    return this.map.has(key) ? (this.map.get(key) as string) : null;
  }
  key(index: number): string | null {
    return [...this.map.keys()][index] ?? null;
  }
  removeItem(key: string): void {
    this.map.delete(key);
  }
  setItem(key: string, value: string): void {
    this.map.set(key, String(value));
  }
}

for (const name of ["localStorage", "sessionStorage"] as const) {
  if (globalThis[name] === undefined) {
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: new MemoryStorage() });
  }
}

afterEach(() => {
  cleanup();
});
