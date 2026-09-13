#!/usr/bin/env bash
#
# Repository verification gate.
#
# Every check a change must survive before it is considered releasable, in one
# command. Stages run in order and stop at the first failure. Nothing here
# reaches the network, and the container stages build only from the pinned
# images the test harnesses already use.
#
#   bash scripts/verify.sh          every stage
#   bash scripts/verify.sh --fast   skip the stages that build container images
#
# Container stages need Docker rootless plus the pinned shell image, which
# test/weston-shell/run.sh builds.
set -euo pipefail

fast=0
case "${1:-}" in
--fast) fast=1 ;;
"") ;;
*)
	printf 'usage: bash scripts/verify.sh [--fast]\n' >&2
	exit 2
	;;
esac

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"
cd -- "${root_dir}"

step() { printf '\n==> %s\n' "$1"; }

# 1. Every Go file this repository owns is gofmt-clean.
step "formatting"
unformatted="$(gofmt -l cmd internal test)"
if [[ -n "${unformatted}" ]]; then
	printf 'error: these files are not gofmt-clean:\n%s\n' "${unformatted}" >&2
	exit 1
fi

# 2. The pinned Weston revision and the files wlvision takes from it still match
# the recorded checksums.
step "pinned Weston baseline"
bash scripts/check-weston-baseline.sh

# 3. Generated bindings are exactly what the protocol XML produces: regenerating
# must change nothing. Comparing hashes rather than a diff against HEAD is what
# makes this true in a working tree with uncommitted work.
generated_fingerprint() {
	find protocol internal/control/generated weston-module/generated -type f -print0 |
		sort -z | xargs -0 sha256sum | sha256sum
}

step "generated bindings"
before="$(generated_fingerprint)"
bash scripts/generate-protocols.sh
after="$(generated_fingerprint)"
if [[ "${before}" != "${after}" ]]; then
	printf 'error: the generated bindings are not what protocol/*.xml produces\n' >&2
	git --no-pager diff --stat -- protocol internal/control/generated weston-module/generated >&2
	exit 1
fi

# 4. The local development replacements are in place, so a test that reaches for
# a sibling module resolves to the checkout rather than to a published version.
step "local development replacements"
bash scripts/check-local-deps.sh

# 5. The whole module builds, vets clean, and passes its tests under the race
# detector. The integration package skips itself without a harness.
step "build, vet and race tests"
go build ./...
go vet ./...
go test -race ./... -count=1

if [[ ${fast} -eq 0 ]]; then
	# 6. The embedded keyboard table is exactly what the pinned image's
	# xkbcommon produces for the layout a session pins.
	step "keyboard table"
	keymap_dir="$(mktemp -d "${TMPDIR:-/tmp}/wlvision-keymap.XXXXXX")"
	bash scripts/generate-keymap.sh "${keymap_dir}/us.json"
	if ! diff -u internal/input/keymap/us.json "${keymap_dir}/us.json"; then
		printf 'error: internal/input/keymap/us.json is not what the generator produces\n' >&2
		rm -rf "${keymap_dir}"
		exit 1
	fi
	rm -rf "${keymap_dir}"

	# 7. The module gate drives the pinned Weston with the wlvision shell and
	# proves the control protocol, capture authorization and credential checks.
	step "module runtime gate"
	bash test/module-runtime/run.sh

	# 8. The session gates run the lifecycle, interaction and vision scenarios
	# against a real rootless engine.
	step "session gates"
	bash test/integration/run.sh
fi

# 9. Release hygiene: a module file with the development replacements dropped,
# exactly as a release resolves it, must contain none.
step "release hygiene"
copy="$(mktemp -d "${TMPDIR:-/tmp}/wlvision-release.XXXXXX")"
trap 'rm -rf "${copy}"' EXIT
awk '!/^replace /' go.mod >"${copy}/go.mod"
cp go.sum "${copy}/go.sum"
go run ./internal/tools/checkreplace "${copy}"

printf '\n==> verification passed\n'
