#!/usr/bin/env bash
#
# Runtime verification for the wlvision Weston module.
#
# Builds the runtime image (pinned Weston plus wlvision-shell), cross-compiles
# the Go control probe, and runs one session inside the container: Weston with
# the wlvision shell, the fixture application, then the probe as the control
# user and again as a foreign user that must be refused.
#
# The probe's own checks are the gate; this script only wires the pieces and
# propagates the exit status.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/../.." && pwd)"
image="wlvision-module-runtime:local"

build_dir="$(mktemp -d "${TMPDIR:-/tmp}/wlvision-module-runtime.XXXXXX")"
cleanup() {
	rm -rf "${build_dir}"
}
trap cleanup EXIT

if ! command -v docker >/dev/null 2>&1; then
	echo "error: docker is required for the module runtime verification" >&2
	exit 1
fi

echo "==> building the control probe"
(
	cd -- "${root_dir}"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "${build_dir}/probe" ./test/controlprobe
)

echo "==> building the runtime image"
docker build -q -t "${image}" -f "${script_dir}/Containerfile" "${root_dir}" >/dev/null

echo "==> running one session"
set +e
docker run --rm -v "${build_dir}/probe:/probe:ro" "${image}" /usr/local/bin/wlvision-session
status=$?
set -e

if [[ ${status} -ne 0 ]]; then
	echo "error: the module runtime verification failed" >&2
	exit "${status}"
fi

echo "==> module runtime verification passed"
