#!/usr/bin/env bash
# Enforces the comment budget in CLAUDE.md: comments are for the non-obvious,
# and they are short. Two limits, both crude on purpose, because the point is
# to stop essays rather than to judge prose.
#
#   MAX_BLOCK  lines in one run of consecutive comment lines
#   MAX_RATIO  percent of a file's non-blank lines that may be comments
#
# Ratio applies only to files above MIN_LINES, so a small file with one long
# header is not flagged twice.
#
# Exempt: license and generated headers, build directives (//go:...), lint
# and eslint pragmas, and the `// Code generated ... DO NOT EDIT.` marker.
set -euo pipefail

MAX_BLOCK="${MAX_BLOCK:-8}"
MAX_RATIO="${MAX_RATIO:-30}"
MIN_LINES="${MIN_LINES:-40}"

cd "$(dirname "$0")/.."

files() {
	git ls-files -z '*.go' 'web/src/*.ts' 'web/src/*.tsx' |
		tr '\0' '\n' |
		grep -v '/migrations/' || true
}

fail=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	grep -q '^// Code generated .* DO NOT EDIT\.$' "$f" && continue

	out=$(awk -v max_block="$MAX_BLOCK" -v max_ratio="$MAX_RATIO" \
		-v min_lines="$MIN_LINES" -v file="$f" '
	function flush(   n) {
		if (run > max_block) {
			printf "%s:%d: comment block of %d lines (max %d)\n", file, start, run, max_block
			bad = 1
		}
		run = 0
	}
	# Directives and pragmas are not prose and never count.
	/^[[:space:]]*\/\/(go|nolint|lint|eslint|@ts-|#)/ { flush(); next }
	/^[[:space:]]*(\/\/|\/\*|\*)/ {
		comments++
		if (run++ == 0) start = FNR
		next
	}
	/[^[:space:]]/ { code++ }
	{ flush() }
	END {
		flush()
		total = comments + code
		if (total >= min_lines) {
			ratio = int(comments * 100 / total)
			if (ratio > max_ratio) {
				printf "%s:1: %d%% of lines are comments (max %d%%)\n", file, ratio, max_ratio
				bad = 1
			}
		}
		exit bad
	}' "$f") || fail=1
	if [ -n "$out" ]; then printf '%s\n' "$out"; fi
done < <(files)

if [ "$fail" -ne 0 ]; then
	cat >&2 <<-EOF

		FAIL: comment budget exceeded (see "Comments" in CLAUDE.md).
		Comments explain what the code cannot: a constraint, a hazard, a
		decision with a discarded alternative. Everything else is noise.
		Cut the block, or move the explanation to docs/.
	EOF
	exit 1
fi
echo "OK: comments"
