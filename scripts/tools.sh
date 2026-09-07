#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "${script_dir}/.." && pwd)

# shellcheck source=tool-versions.env
. "${script_dir}/tool-versions.env"

tools_bin_dir=${TOOLS_BIN_DIR:-"${repo_root}/.tools/bin"}
toolchain=$(awk '$1 == "toolchain" { print $2; exit }' "${repo_root}/go.mod")

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

usage() {
	printf 'Usage: %s {install|check} [goimports|golangci-lint|staticcheck|govulncheck ...]\n' "$0" >&2
	exit 2
}

set_tool() {
	case "$1" in
		goimports)
			tool_name=goimports
			tool_package=${GOIMPORTS_PACKAGE}
			tool_module=${GOIMPORTS_MODULE}
			tool_version=${GOIMPORTS_VERSION}
			;;
		golangci-lint)
			tool_name=golangci-lint
			tool_package=${GOLANGCI_LINT_PACKAGE}
			tool_module=${GOLANGCI_LINT_MODULE}
			tool_version=${GOLANGCI_LINT_VERSION}
			;;
		staticcheck)
			tool_name=staticcheck
			tool_package=${STATICCHECK_PACKAGE}
			tool_module=${STATICCHECK_MODULE}
			tool_version=${STATICCHECK_VERSION}
			;;
		govulncheck)
			tool_name=govulncheck
			tool_package=${GOVULNCHECK_PACKAGE}
			tool_module=${GOVULNCHECK_MODULE}
			tool_version=${GOVULNCHECK_VERSION}
			;;
		*)
			fail "unknown Go tool '$1'"
			;;
	esac
	tool_path=${tools_bin_dir}/${tool_name}
}

check_go() {
	[ -n "${toolchain}" ] || fail "go.mod does not declare a toolchain"
	command -v go >/dev/null 2>&1 || fail "go is required"

	export GOTOOLCHAIN=${toolchain}
	go_version=$(go env GOVERSION 2>/dev/null) || fail "could not start the ${toolchain} toolchain"
	[ "${go_version}" = "${toolchain}" ] || fail "expected ${toolchain}, got ${go_version}"

	goroot=$(go env GOROOT 2>/dev/null) || fail "could not locate gofmt for ${toolchain}"
	[ -x "${goroot}/bin/gofmt" ] || fail "gofmt is missing from ${toolchain}"
}

check_runtime_commands() {
	command -v node >/dev/null 2>&1 || fail "node is required (Node.js 22 or newer)"
	command -v npm >/dev/null 2>&1 || fail "npm is required"

	node_version=$(node --version 2>/dev/null) || fail "could not run node"
	node_major=${node_version#v}
	node_major=${node_major%%.*}
	case "${node_major}" in
		''|*[!0-9]*) fail "could not parse Node.js version '${node_version}'" ;;
	esac
	[ "${node_major}" -ge 22 ] || fail "Node.js 22 or newer is required, got ${node_version}"
	npm --version >/dev/null 2>&1 || fail "could not run npm"
}

check_tool() {
	set_tool "$1"
	[ -x "${tool_path}" ] || fail "${tool_name} ${tool_version} is not installed at ${tool_path}; run 'make tools'"

	installed_version=$(go version -m "${tool_path}" 2>/dev/null | awk -v module="${tool_module}" '$1 == "mod" && $2 == module { print $3; exit }')
	[ -n "${installed_version}" ] || fail "could not determine the version of ${tool_path}; run 'make tools'"
	[ "${installed_version}" = "${tool_version}" ] || fail "${tool_name} at ${tool_path} is ${installed_version}, expected ${tool_version}; run 'make tools'"
}

install_tool() {
	set_tool "$1"
	printf 'Installing %s %s\n' "${tool_name}" "${tool_version}"
	mkdir -p "${tools_bin_dir}"
	GOBIN="${tools_bin_dir}" go install "${tool_package}@${tool_version}"
	check_tool "${tool_name}"
}

[ "$#" -ge 1 ] || usage
command_name=$1
shift

case "${command_name}" in
	install|check) ;;
	*) usage ;;
esac

check_go

if [ "$#" -eq 0 ]; then
	check_all=true
	set -- goimports golangci-lint staticcheck govulncheck
else
	check_all=false
fi

if [ "${command_name}" = install ]; then
	for requested_tool in "$@"; do
		install_tool "${requested_tool}"
	done
	printf 'OK: pinned Go tools are installed in %s\n' "${tools_bin_dir}"
else
	if [ "${check_all}" = true ]; then
		check_runtime_commands
	fi
	for requested_tool in "$@"; do
		check_tool "${requested_tool}"
	done
fi
