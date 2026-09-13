#!/usr/bin/env bash
#
# Check that the local development replacements are in place.
#
# Go replacements are not transitive: a replace in wlvision does not apply to
# the requirements of the sibling modules, so this script inspects the module
# graph of BOTH wlvision and ../libwldevices-go through the checkreplace
# helper. It requires exactly the expected replacements and rejects any other
# filesystem replacement, without depending on jq or on the formatting of the
# raw go.mod text.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"
libwldevices_dir="$(cd -- "${root_dir}/../libwldevices-go" && pwd)"

# run_check <module-dir> [-expect OLD=NEW]...
run_check() {
	local target_dir="$1"
	shift
	(cd -- "${root_dir}" && go run ./internal/tools/checkreplace "$@" "${target_dir}")
}

run_check "${root_dir}" \
	-expect "github.com/bnema/wlturbo=../wlturbo" \
	-expect "github.com/bnema/libwldevices-go=../libwldevices-go"

# The sibling module must replace WLTurbo as well, because replacements do not
# apply to a dependency's own module graph.
run_check "${libwldevices_dir}" -expect "github.com/bnema/wlturbo=../wlturbo"
