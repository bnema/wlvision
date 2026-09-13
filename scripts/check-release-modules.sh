#!/usr/bin/env bash
#
# Release guard: fail if any module still has a filesystem replacement.
#
# Local development replaces point at the sibling checkouts ../wlturbo and
# ../libwldevices-go. Such replaces must never ship, so this script runs
# ./internal/tools/checkreplace on wlvision and on ../libwldevices-go (which
# has its own replace for wlturbo). It exits non-zero while local development
# replacements are active and 0 on clean, release-resolved module files.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"

cd -- "${root_dir}"
exec go run ./internal/tools/checkreplace "${root_dir}" "${root_dir}/../libwldevices-go"
