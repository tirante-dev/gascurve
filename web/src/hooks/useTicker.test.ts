import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useTicker } from "./useTicker";

describe("useTicker", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(1_000_000);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("advances on the interval and pauses while hidden", () => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    const { result } = renderHook(() => useTicker(100));
    expect(result.current).toBe(1_000_000);
    act(() => {
      vi.advanceTimersByTime(250);
    });
    expect(result.current).toBe(1_000_200);
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(result.current).toBe(1_000_200);
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    act(() => {
      vi.advanceTimersByTime(100);
    });
    expect(result.current).toBe(1_001_350);
  });

  it("holds the first reading and starts no timer at an interval of zero", () => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    const { result } = renderHook(() => useTicker(0));
    expect(result.current).toBe(1_000_000);
    act(() => {
      vi.advanceTimersByTime(5_000);
    });
    expect(result.current).toBe(1_000_000);
    expect(vi.getTimerCount()).toBe(0);
  });
});
