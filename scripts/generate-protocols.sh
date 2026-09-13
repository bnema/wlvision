#!/usr/bin/env bash
#
# Regenerate every protocol binding from protocol/*.xml.
#
# Go client bindings come from the WLTurbo-backed scanner in LibWL Devices; the
# C server bindings for the compositor module come from wayland-scanner. Both
# write into committed paths and are expected to be byte-stable: running this
# twice must produce no diff, and CI fails when a regenerated file drifts.
#
# The capture protocol is a copy of a file from the pinned Weston revision, so
# this refuses to run when that pin no longer matches.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"
cd -- "${root_dir}"

bash scripts/check-weston-baseline.sh

if ! command -v wayland-scanner >/dev/null 2>&1; then
	echo "error: wayland-scanner is required to generate the C server bindings" >&2
	exit 1
fi

scanner() {
	go run github.com/bnema/libwldevices-go/scanner/cmd/wayland-scanner "$@"
}

generate_go() {
	local package="$1" output="$2" xml="$3"
	mkdir -p "$(dirname -- "${output}")"
	scanner -p "${package}" -o "${output}" "${xml}"
}

generate_c() {
	local xml="$1" header="$2" code="$3"
	mkdir -p "$(dirname -- "${header}")"
	wayland-scanner server-header "${xml}" "${header}"
	wayland-scanner private-code "${xml}" "${code}"
}

# Project-owned control protocol: Go client bindings for the resident
# controller, C server bindings for the compositor module.
generate_go generated internal/control/generated/wlvision_control.go protocol/wlvision-control.xml
generate_c protocol/wlvision-control.xml \
	weston-module/generated/wlvision-control-server.h \
	weston-module/generated/wlvision-control-protocol.c

# Pinned Weston capture protocol: the same treatment, from the vendored copy.
generate_go generated internal/capture/generated/weston_output_capture.go protocol/weston-output-capture.xml
generate_c protocol/weston-output-capture.xml \
	weston-module/generated/weston-output-capture-server.h \
	weston-module/generated/weston-output-capture-protocol.c

# The scanner writes with restrictive permissions; committed sources are readable.
chmod 644 internal/control/generated/wlvision_control.go internal/capture/generated/weston_output_capture.go

gofmt -l internal/control/generated internal/capture/generated

echo "protocols regenerated from protocol/*.xml"
