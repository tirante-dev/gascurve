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
# Line classification tracks `/* */` state, so a block comment whose
# continuation lines do not start with `*` and a JSX `{/* ... */}` wrapper both
# count. A `/*` is only an opener at the start of a line (after optional
# whitespace and an optional `{`), which keeps one inside a string literal from
# putting the scanner into a block it never leaves.
#
# Exempt: a leading copyright or SPDX header (found in a first pass and left out
# of both limits, so a legal header can never fail the build), generated files,
# build directives (//go:...) and lint pragmas.
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

	# The file is read twice: once to measure a leading license header, once to
	# apply the limits to everything after it.
	out=$(awk -v max_block="$MAX_BLOCK" -v max_ratio="$MAX_RATIO" \
		-v min_lines="$MIN_LINES" -v file="$f" '
	# "c" comment, "b" blank, "x" code. Maintains /* */ state across lines.
	function classify(   line) {
		line = $0
		if (line ~ /^[[:space:]]*$/) return "b"
		if (inblock) {
			if (index(line, "*/")) inblock = 0
			return "c"
		}
		if (line ~ /^[[:space:]]*\/\//) return "c"
		if (line ~ /^[[:space:]]*[{]?[[:space:]]*\/\*/) {
			if (index(substr(line, index(line, "/*") + 2), "*/") == 0) inblock = 1
			return "c"
		}
		return "x"
	}
	function flush() {
		if (run > max_block) {
			printf "%s:%d: comment block of %d lines (max %d)\n", file, start, run, max_block
			bad = 1
		}
		run = 0
	}

	# Pass one: the leading comment block, and whether it is a legal header.
	NR == FNR {
		if (!scanning) next
		k = classify()
		if (k == "c" && FNR == header + 1) {
			header = FNR
			if ($0 ~ /[Cc]opyright|SPDX-License-Identifier/) licensed = 1
		} else {
			scanning = 0
		}
		next
	}

	FNR == 1 {
		inblock = 0
		skip = licensed ? header : 0
	}
	FNR <= skip { next }

	{ k = classify() }
	k == "b" { flush(); next }
	# Directives and pragmas are not prose and never count.
	k == "c" && /^[[:space:]]*\/\/(go|nolint|lint|eslint|@ts-|#)/ { flush(); next }
	k == "c" {
		comments++
		if (run++ == 0) start = FNR
		next
	}
	{ code_lines++; flush() }

	BEGIN { scanning = 1 }
	END {
		flush()
		total = comments + code_lines
		if (total >= min_lines && comments * 100 > max_ratio * total) {
			printf "%s:1: %.1f%% of lines are comments (max %d%%)\n", \
				file, comments * 100 / total, max_ratio
			bad = 1
		}
		exit bad
	}' "$f" "$f") || fail=1
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
