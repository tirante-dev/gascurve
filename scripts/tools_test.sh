#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "${script_dir}/.." && pwd)

# `make ci` runs test-tooling and this script runs `make ci`, so the only thing
# keeping that from looping forever is the preflight failing. Turn a regression
# there into an error instead of a hang.
if [ "${GASCURVE_TOOLS_TEST:-}" = 1 ]; then
	printf 'FAIL: tools_test.sh re-entered itself, so `make ci` reached test-tooling with a failing preflight\n' >&2
	exit 1
fi
GASCURVE_TOOLS_TEST=1
export GASCURVE_TOOLS_TEST

# shellcheck source=tool-versions.env
. "${script_dir}/tool-versions.env"

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT HUP INT TERM

fake_bin=${work}/fake-bin
tools_bin=${work}/tools-bin
mkdir -p "${fake_bin}" "${tools_bin}"

write_executable() {
	path=$1
	shift
	printf '%s\n' '#!/bin/sh' "$@" > "${path}"
	chmod +x "${path}"
}

write_executable "${fake_bin}/go" \
	'case "$1:$2" in' \
	'  env:GOVERSION) printf "%s\\n" "${GOTOOLCHAIN}" ;;' \
	'  env:GOROOT) printf "%s\\n" "${FAKE_GOROOT}" ;;' \
	'  version:-m)' \
	'    name=${3##*/}' \
	'    case "${name}" in' \
	'      goimports) module="golang.org/x/tools"; version=${FAKE_GOIMPORTS_VERSION} ;;' \
	'      golangci-lint) module="github.com/golangci/golangci-lint/v2"; version=${FAKE_GOLANGCI_LINT_VERSION} ;;' \
	'      staticcheck) module="honnef.co/go/tools"; version=${FAKE_STATICCHECK_VERSION} ;;' \
	'      govulncheck) module="golang.org/x/vuln"; version=${FAKE_GOVULNCHECK_VERSION} ;;' \
	'      *) exit 1 ;;' \
	'    esac' \
	'    printf "%s:\\n\\tmod\\t%s\\t%s\\n" "$3" "${module}" "${version}"' \
	'    ;;' \
	'  *) exit 1 ;;' \
	'esac'
write_executable "${fake_bin}/node" 'printf "%s\\n" "v22.0.0"'
write_executable "${fake_bin}/npm" 'printf "%s\\n" "10.0.0"'
mkdir -p "${work}/goroot/bin"
write_executable "${work}/goroot/bin/gofmt" 'exit 0'

for tool in goimports golangci-lint staticcheck govulncheck; do
	write_executable "${tools_bin}/${tool}" 'exit 0'
done

export PATH=${fake_bin}:$PATH
export FAKE_GOROOT=${work}/goroot
export FAKE_GOIMPORTS_VERSION=${GOIMPORTS_VERSION}
export FAKE_GOLANGCI_LINT_VERSION=${GOLANGCI_LINT_VERSION}
export FAKE_STATICCHECK_VERSION=${STATICCHECK_VERSION}
export FAKE_GOVULNCHECK_VERSION=${GOVULNCHECK_VERSION}

TOOLS_BIN_DIR=${tools_bin} "${script_dir}/tools.sh" check

FAKE_GOLANGCI_LINT_VERSION=v0.0.0
export FAKE_GOLANGCI_LINT_VERSION
if output=$(TOOLS_BIN_DIR=${tools_bin} "${script_dir}/tools.sh" check golangci-lint 2>&1); then
	printf 'FAIL: version mismatch unexpectedly passed\n' >&2
	exit 1
fi
case "${output}" in
	*"expected ${GOLANGCI_LINT_VERSION}"*) ;;
	*)
		printf 'FAIL: version mismatch did not report the expected pin:\n%s\n' "${output}" >&2
		exit 1
		;;
esac
FAKE_GOLANGCI_LINT_VERSION=${GOLANGCI_LINT_VERSION}
export FAKE_GOLANGCI_LINT_VERSION

missing_bin=${work}/missing-bin
mkdir -p "${missing_bin}"
if output=$(make --no-print-directory -s -C "${repo_root}" fmt-check TOOLS_BIN_DIR="${missing_bin}" 2>&1); then
	printf 'FAIL: fmt-check unexpectedly passed without goimports\n' >&2
	exit 1
fi
case "${output}" in
	*"goimports ${GOIMPORTS_VERSION} is not installed"*) ;;
	*)
		printf 'FAIL: fmt-check did not report missing goimports:\n%s\n' "${output}" >&2
		exit 1
		;;
esac
case "${output}" in
	*"OK: formatting"*)
		printf 'FAIL: fmt-check printed success after a missing-command error\n' >&2
		exit 1
		;;
esac

if output=$(make --no-print-directory -s -C "${repo_root}" ci TOOLS_BIN_DIR="${missing_bin}" 2>&1); then
	printf 'FAIL: ci unexpectedly passed without its required tools\n' >&2
	exit 1
fi
case "${output}" in
	*"goimports ${GOIMPORTS_VERSION} is not installed"*) ;;
	*)
		printf 'FAIL: ci preflight did not report missing tools:\n%s\n' "${output}" >&2
		exit 1
		;;
esac

# Only the tool the first check needs is installed. A preflight that is a real
# phase boundary rejects the run before any check produces output; a plain
# prerequisite list would let fmt-check pass first and only fail at lint.
partial_bin=${work}/partial-bin
mkdir -p "${partial_bin}"
cp "${tools_bin}/goimports" "${partial_bin}/goimports"

if output=$(make --no-print-directory -s -C "${repo_root}" ci TOOLS_BIN_DIR="${partial_bin}" 2>&1); then
	printf 'FAIL: ci unexpectedly passed with an incomplete tool set\n' >&2
	exit 1
fi
case "${output}" in
	*"golangci-lint ${GOLANGCI_LINT_VERSION} is not installed"*) ;;
	*)
		printf 'FAIL: ci preflight did not report the missing golangci-lint:\n%s\n' "${output}" >&2
		exit 1
		;;
esac
case "${output}" in
	*"OK: formatting"*|*"All CI checks passed"*)
		printf 'FAIL: ci ran checks after its preflight failed:\n%s\n' "${output}" >&2
		exit 1
		;;
esac

printf 'OK: tooling checks\n'
