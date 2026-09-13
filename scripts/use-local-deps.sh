#!/usr/bin/env bash
#
# Apply the local development replacements for the sibling checkouts.
#
# Go module replacements are not transitive, so both wlvision and
# ../libwldevices-go need their own replace for wlturbo. The replacements use
# the literal relative paths ../wlturbo and ../libwldevices-go, which resolve
# against the directory of each go.mod file, not against the caller's cwd.
#
# This never touches HOME or the global Go configuration.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"

wlvision_dir="${root_dir}"
libwldevices_dir="$(cd -- "${root_dir}/../libwldevices-go" && pwd)"

(
	cd -- "${wlvision_dir}"
	go mod edit -replace github.com/bnema/wlturbo=../wlturbo
	go mod edit -replace github.com/bnema/libwldevices-go=../libwldevices-go
)

(
	cd -- "${libwldevices_dir}"
	go mod edit -replace github.com/bnema/wlturbo=../wlturbo
)
