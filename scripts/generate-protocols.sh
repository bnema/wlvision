#!/usr/bin/env bash
#
# Regenerate every protocol binding from protocol/*.xml.
#
# Go client bindings come from the WLTurbo-backed scanner in LibWL Devices; the
# C server bindings for the compositor module come from wayland-scanner. Both
# write into committed paths and are expected to be byte-stable: running this
# twice must produce no diff, and CI fails when a regenerated file drifts.
#
# wayland-scanner changes its output between releases, so the C bindings can
# only be reproduced by the release that generated them. That release is pinned
# below. A host carrying another one regenerates the Go bindings and leaves the
# C files as they are, saying so, rather than failing a comparison it cannot
# win: distributions ship older scanners than the one this repository pins.
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

# The release whose code generation the committed C bindings are. The tool
# reports its version on stderr.
wayland_scanner_pin="1.26.0"
host_scanner="$(wayland-scanner --version 2>&1 | awk '{print $2}')"
check_c=1
if [[ "${host_scanner}" != "${wayland_scanner_pin}" ]]; then
	check_c=0
	printf 'note: wayland-scanner %s is not the pinned %s; the C bindings are not regenerated\n' \
		"${host_scanner:-unknown}" "${wayland_scanner_pin}" >&2
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
	[[ "${check_c}" -eq 1 ]] || return 0
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

if [[ "${check_c}" -eq 1 ]]; then
	echo "protocols regenerated from protocol/*.xml"
else
	echo "Go bindings regenerated; C bindings left to wayland-scanner ${host_scanner:-unknown}"
fi
