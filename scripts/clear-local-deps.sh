#!/usr/bin/env bash
#
# Drop the local development replacements applied by use-local-deps.sh.
#
# `go mod edit -dropreplace` only removes the replace directives, so the
# module files return to their committed, release-resolved state (requires and
# go directives are untouched). The sibling module is tidied afterwards only to
# restore the go.sum entries that a filesystem replacement makes unnecessary.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"

wlvision_dir="${root_dir}"
libwldevices_dir="$(cd -- "${root_dir}/../libwldevices-go" && pwd)"

(
	cd -- "${wlvision_dir}"
	go mod edit -dropreplace github.com/bnema/wlturbo
	go mod edit -dropreplace github.com/bnema/libwldevices-go
)

(
	cd -- "${libwldevices_dir}"
	go mod edit -dropreplace github.com/bnema/wlturbo
	# Without the replacement the published requirement is in play again, so
	# refresh go.sum to the state the committed module file expects.
	go mod tidy
)
