"use client";

/** One choice in a segmented control: the value it selects and the word on it. */
export type RangeOption<T extends string> = { value: T; label: string };

/**
 * The segmented range control. Every range picker on the page is this one
 * component, so the hero's chart and the history section read as the same
 * control rather than as two that happen to look alike.
 */
export function RangeTabs<T extends string>({
  options,
  value,
  onChange,
  label,
  loading,
}: {
  options: readonly RangeOption<T>[];
  value: T;
  onChange: (value: T) => void;
  label: string;
  loading?: boolean;
}) {
  return (
    <div className="flex items-center gap-3" role="tablist" aria-label={label}>
      <div className="vw-control inline-flex p-0.5">
        {options.map((option) => {
          const selected = option.value === value;
          return (
            <button
              key={option.value}
              type="button"
              role="tab"
              aria-selected={selected}
              className={`num rounded-full px-3 py-1 text-sm ${selected ? "vw-tab-on bg-accent font-medium text-accent-ink" : "text-ink-2 hover:text-ink"}`}
              onClick={() => onChange(option.value)}
            >
              {option.label}
            </button>
          );
        })}
      </div>
      {loading ? <span className="text-xs text-ink-3">updating</span> : null}
    </div>
  );
}
