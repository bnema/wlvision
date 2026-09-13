#!/usr/bin/env bash
#
# Session gates.
#
# Builds the session image from the pinned Weston shell image, cross-compiles the
# wlvision binaries, and runs every gate that needs a real rootless engine: the
# lifecycle, the interaction and vision scenarios, the isolation matrix and the
# V1 acceptance walkthrough.
#
# The tests create their own sessions, so this script only wires the pieces and
# propagates the exit status.
#
# Requirements: docker (rootless) and the pinned Weston shell image, which
# test/weston-shell/run.sh builds.
#
# Environment:
#   WLVISION_TEST_COUNT      repeat every gate this many times (default 1)
#   WLVISION_TEST_MANIFEST   set to 1 to also build an image from a manifest and
#                            run a session from it; that stage needs the network
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/../.." && pwd)"
image="wlvision-session:local"
test_count="${WLVISION_TEST_COUNT:-1}"
manifest_stage="${WLVISION_TEST_MANIFEST:-0}"

if ! command -v docker >/dev/null 2>&1; then
	echo "error: docker is required for the session gates" >&2
	exit 1
fi

build_dir="$(mktemp -d "${TMPDIR:-/tmp}/wlvision-session.XXXXXX")"
state_dir="$(mktemp -d "${TMPDIR:-/tmp}/wlvision-state.XXXXXX")"
# A failing gate keeps its build context and session state, which is what makes
# a failure diagnosable after the fact; a passing one leaves nothing behind.
cleanup() {
	if [[ $? -eq 0 ]]; then
		rm -rf "${build_dir}" "${state_dir}"
	else
		printf 'artifacts kept for diagnosis: build=%s state=%s\n' "${build_dir}" "${state_dir}" >&2
	fi
}
trap cleanup EXIT

# gate <name> <go test pattern> runs one gate and fails when its pattern matches
# no test at all: a stage that silently runs nothing is worse than no stage.
gate() {
	local name="$1"
	local pattern="$2"
	local output

	echo "==> running the ${name} gate"
	if ! output="$(
		cd -- "${root_dir}"
		WLVISION_TEST_IMAGE="${image}" \
		WLVISION_TEST_CLI="${build_dir}/bin/wlvision" \
		WLVISION_TEST_STATE="${state_dir}" \
			go test ./test/integration -run "${pattern}" -count="${test_count}" -v 2>&1
	)"; then
		printf '%s\n' "${output}" >&2
		echo "error: the ${name} gate failed" >&2
		exit 1
	fi
	if grep -q 'no tests to run' <<<"${output}"; then
		printf '%s\n' "${output}" >&2
		echo "error: the ${name} gate matched no test; its pattern is stale" >&2
		exit 1
	fi
	printf '%s\n' "${output}" | grep -E '^(=== RUN|--- (PASS|FAIL)|ok|FAIL|PASS)' || true
}

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

gate "lifecycle" 'TestLifecycle'
gate "interaction and vision" 'TestResizeCommit|TestVisionInput|TestAnimationBurst|TestQuietStability'
gate "isolation matrix" 'TestIsolationMatrix'
gate "V1 acceptance" 'TestV1Acceptance'

if [[ "${manifest_stage}" == "1" ]]; then
	# This gate builds an image from a manifest, which installs packages and so
	# needs the network; the session it then runs has none.
	gate "manifest build then offline run" 'TestManifestBuildThenOfflineRun'
fi

echo "==> session gates passed"
