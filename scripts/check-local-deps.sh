#!/usr/bin/env bash
#
# Check that the local development replacements are in place.
#
# Go replacements are not transitive: a replace in wlvision does not apply to
# the requirements of ../libwldevices-go, so this script inspects the module
# graph of BOTH modules. It reads each module with `go mod edit -json` and
# matches the parsed Replace entries with jq, so it does not depend on the
# formatting of the raw go.mod text. Every unmet expectation is reported and
# the script exits non-zero.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"

if ! command -v jq >/dev/null 2>&1; then
	echo "error: $(basename -- "${BASH_SOURCE[0]}") requires jq to parse go mod edit -json output" >&2
	exit 1
fi

status=0

# expect_replace <module-dir> <old-module-path> <new-filesystem-path>
expect_replace() {
	local dir="$1" old="$2" new="$3" json
	json="$(cd -- "$dir" && go mod edit -json)"
	if ! jq -e --arg old "$old" --arg new "$new" \
		'any(.Replace[]?; .Old.Path == $old and .New.Path == $new and (.New.Version // "") == "")' \
		<<<"${json}" >/dev/null; then
		printf 'error: %s: expected replace %s => %s is missing\n' "$dir" "$old" "$new" >&2
		status=1
	fi
}

expect_replace "${root_dir}" github.com/bnema/wlturbo ../wlturbo
expect_replace "${root_dir}" github.com/bnema/libwldevices-go ../libwldevices-go
expect_replace "${root_dir}/../libwldevices-go" github.com/bnema/wlturbo ../wlturbo

exit "${status}"
