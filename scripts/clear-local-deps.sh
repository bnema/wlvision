#!/usr/bin/env bash
#
# Drop the local development replacements applied by use-local-deps.sh.
#
# `go mod edit -dropreplace` only removes the replace directives; no `go mod
# tidy` is run, so the module files return byte-for-byte to their committed,
# release-resolved state (requires and go directives are untouched).
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
)
