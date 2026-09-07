"use client";

/** One choice in a segmented control: the value it selects and the word on it. */
export type RangeOption<T extends string> = { value: T; label: string };

/**
 * The segmented range control. Every range picker on the page is this one component, so the hero's chart
 * and the history section read as the same control.
 *
 * It is a labelled group of toggle buttons, not a tablist: a tablist promises a keyboard model that a
 * control switching what one chart draws does not have and should not fake. The buttons wrap rather than
 * overflow, so five ranges stay inside the card on a phone.
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
    <div className="flex min-w-0 max-w-full flex-col items-start gap-1">
      <div className="vw-control flex max-w-full flex-wrap gap-y-0.5 p-0.5" role="group" aria-label={label}>
        {options.map((option) => {
          const selected = option.value === value;
          return (
            <button
              key={option.value}
              type="button"
              aria-pressed={selected}
              className={`num rounded-full px-3 py-1 text-sm ${selected ? "vw-tab-on bg-accent font-medium text-accent-ink" : "text-ink-2 hover:text-ink"}`}
              onClick={() => onChange(option.value)}
            >
              {option.label}
            </button>
          );
        })}
      </div>
      {loading ? (
        <span className="text-xs text-ink-3" role="status">
          updating
        </span>
      ) : null}
    </div>
  );
}
