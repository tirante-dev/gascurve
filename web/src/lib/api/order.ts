// The one place point order is settled. Every range chart downstream (gap
// detection, partial-bucket classification, set boundaries, first and last
// point) reads the points as a sequence in time, and the contract does not
// promise an order. Normalising here, at the client boundary, means no chart
// has to sort defensively and none of them can disagree about which point is
// last.

/** True when every point sits at or after the one before it. */
export function isAscendingByTime(points: readonly { t: number }[]): boolean {
  for (let i = 1; i < points.length; i++) {
    if (points[i].t < points[i - 1].t) return false;
  }
  return true;
}

/**
 * `series` with its points in ascending time order. Sorting is stable, so two
 * points sharing a timestamp keep the order the api sent them in. The series
 * itself is handed back unchanged when the points already ascend (the ordinary
 * case), so nothing downstream sees a new identity for data that did not move.
 */
export function orderPoints<P extends { t: number }, S extends { points: P[] }>(series: S): S {
  const points = series.points;
  if (!Array.isArray(points) || isAscendingByTime(points)) return series;
  return { ...series, points: [...points].sort((a, b) => a.t - b.t) };
}
