#!/usr/bin/env bash
#
# generate-keymap.sh regenerates internal/input/keymap/us.json, the keyboard
# artifact the host-side input service resolves text and key names with.
#
# The artifact is compiled inside the local session image, so the host needs no
# xkbcommon headers and no compiler, and the keymap it describes is the one the
# supervisor pins for the session's own keyboard: rules=evdev, model=pc105,
# layout=us, variant="" and options="". Those five values are the contract
# between this generator and the session's compositor; changing either side
# alone would make the service send keycodes the session does not have.
#
# The image must already be present. This script never builds, pulls or
# otherwise fetches it: it fails loudly when the image is missing and when the
# compile fails, and it cleans up its temporary directory.
#
# Usage: scripts/generate-keymap.sh [output-path]
#
# With no argument it writes the committed artifact, internal/input/keymap/
# us.json, relative to the repository root regardless of the current
# directory. Pass a path to write elsewhere, for example to diff a regenerated
# artifact against the committed one:
#
#     scripts/generate-keymap.sh /tmp/us.json && diff /tmp/us.json internal/input/keymap/us.json
set -euo pipefail

image="${WLVISION_KEYMAP_IMAGE:-wlvision-weston-shell:local}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-internal/input/keymap/us.json}"

if [[ "$out" != /* ]]; then
  out="$root/$out"
fi

if ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "generate-keymap: image $image is missing; build or import it before regenerating the keymap" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cp "$root/keymap/keymap-dump.c" "$tmp/keymap-dump.c"

# The compile and the dump both happen inside the image. -euc makes a failure in
# either step fail the container, and set -euo pipefail carries it out here.
docker run --rm -v "$tmp:/build" "$image" sh -euc '
  cd /build
  gcc -std=c11 -Wall -Wextra -O2 -o keymap-dump keymap-dump.c $(pkg-config --cflags --libs xkbcommon)
  ./keymap-dump > us.json
'

if [[ ! -s "$tmp/us.json" ]]; then
  echo "generate-keymap: the dump produced no artifact" >&2
  exit 1
fi

mkdir -p "$(dirname "$out")"
cp "$tmp/us.json" "$out"
echo "generate-keymap: wrote $out"
