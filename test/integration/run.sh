#!/usr/bin/env bash
#
# Session lifecycle gate.
#
# Builds the session image from the pinned Weston shell image, cross-compiles
# the four wlvision binaries, and runs the lifecycle test against a real
# rootless engine. The test creates its own session, so the script only wires
# the pieces and propagates the exit status.
#
# Requirements: docker (rootless), the pinned Weston shell image
# (test/weston-shell/run.sh builds it), and nothing else.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/../.." && pwd)"
image="wlvision-session:local"

if ! command -v docker >/dev/null 2>&1; then
	echo "error: docker is required for the session lifecycle gate" >&2
	exit 1
fi

build_dir="$(mktemp -d "${TMPDIR:-/tmp}/wlvision-session.XXXXXX")"
state_dir="$(mktemp -d "${TMPDIR:-/tmp}/wlvision-state.XXXXXX")"
cleanup() {
	rm -rf "${build_dir}" "${state_dir}"
}
trap cleanup EXIT

echo "==> building the session binaries"
mkdir -p "${build_dir}/bin"
(
	cd -- "${root_dir}"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "${build_dir}/bin/" ./cmd/...
)

echo "==> assembling the image context"
cp -r "${root_dir}/weston-module" "${build_dir}/weston-module"

echo "==> building the session image"
docker build -q -t "${image}" -f "${root_dir}/images/arch/Containerfile" "${build_dir}" >/dev/null

echo "==> running the lifecycle test"
(
	cd -- "${root_dir}"
	WLVISION_TEST_IMAGE="${image}" \
	WLVISION_TEST_CLI="${build_dir}/bin/wlvision" \
	WLVISION_TEST_STATE="${state_dir}" \
		go test ./test/integration -run TestLifecycle -count=1 -v
)

echo "==> session lifecycle gate passed"
