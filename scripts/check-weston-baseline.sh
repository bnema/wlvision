#!/usr/bin/env bash
#
# Verify the pinned Weston baseline.
#
# images/arch/weston.lock must record a full commit hash, the SHA-256 of the
# source archive, the libweston ABI major, and the SHA-256 of every Weston file
# wlvision depends on. This script fails while any of those are missing or
# malformed, and fails when the vendored copy of the output-capture protocol no
# longer matches the checksum recorded for it. No image or module build may run
# against an unverified baseline.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd -- "${script_dir}/.." && pwd)"
lock_file="${root_dir}/images/arch/weston.lock"

status=0

fail() {
	printf 'error: %s\n' "$1" >&2
	status=1
}

if [[ ! -f "${lock_file}" ]]; then
	printf 'error: %s is missing; select and record a Weston revision before building anything\n' "${lock_file}" >&2
	exit 1
fi

# lock_value <key> prints the value of a key=value line, or nothing.
lock_value() {
	local key="$1"
	sed -n "s/^${key}=//p" "${lock_file}" | head -n 1
}

require_value() {
	local key="$1"
	local value
	value="$(lock_value "${key}")"
	if [[ -z "${value}" ]]; then
		fail "${lock_file}: required key '${key}' is missing or empty"
	fi
	printf '%s' "${value}"
}

require_pattern() {
	local key="$1" pattern="$2" description="$3"
	local value
	value="$(require_value "${key}")"
	if [[ -n "${value}" ]] && ! [[ "${value}" =~ ${pattern} ]]; then
		fail "${lock_file}: ${key}='${value}' is not ${description}"
	fi
}

# The recorded revision must be a full commit, never a branch or a short hash.
require_pattern weston_commit '^[0-9a-f]{40}$' 'a full 40-character commit hash'
require_pattern weston_version '^[0-9]+\.[0-9]+\.[0-9]+$' 'a released version'
require_pattern source_archive_sha256 '^[0-9a-f]{64}$' 'a SHA-256 digest'
require_pattern source_archive_url '^https://' 'an https URL'
require_pattern libweston_abi_major '^[0-9]+$' 'an integer ABI major'

# Every file the module depends on is pinned by digest. Files that exist in this
# repository are verified; the rest are verified inside the probe image against
# the pinned source archive.
while IFS='=' read -r key value; do
	case "${key}" in
	file_sha256/*)
		relative_path="${key#file_sha256/}"
		if [[ ! "${value}" =~ ^[0-9a-f]{64}$ ]]; then
			fail "${lock_file}: ${key}='${value}' is not a SHA-256 digest"
			continue
		fi
		if [[ -f "${root_dir}/${relative_path}" ]]; then
			actual="$(sha256sum "${root_dir}/${relative_path}" | cut -d' ' -f1)"
			if [[ "${actual}" != "${value}" ]]; then
				fail "${relative_path} does not match the pinned Weston revision: recorded ${value}, actual ${actual}"
			fi
		fi
		;;
	esac
done <"${lock_file}"

if ! grep -q '^file_sha256/protocol/weston-output-capture.xml=' "${lock_file}"; then
	fail "${lock_file}: the output-capture protocol is not pinned by digest"
fi

if [[ ${status} -eq 0 ]]; then
	printf 'weston baseline verified: %s (%s), libweston ABI %s\n' \
		"$(lock_value weston_version)" "$(lock_value weston_commit)" "$(lock_value libweston_abi_major)"
fi

exit "${status}"
