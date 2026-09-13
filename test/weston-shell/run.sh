#!/usr/bin/env bash
#
# wlvision - end-to-end run of the purpose-built Weston shell probe.
#
# Proves Option A of docs/weston-baseline.md: Weston runs with
# shell=wlvision-shell-probe (a shell module owned by wlvision), an
# xdg_toplevel client maps, the shell activates it, and a resize request is
# acknowledged by a matching client buffer.
#
#   bash test/weston-shell/run.sh
#
# The inputs stage is the only stage allowed to use the network.  The
# verification stage is built with --network=none, so the run fails rather
# than fetch anything if a layer is missing.  The container is always removed.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
inputs_image="wlvision-weston-inputs:local"
shell_image="wlvision-weston-shell:local"
container_name="wlvision-shell-probe-$$"
output="$(mktemp)"

cleanup() {
	docker rm -f "${container_name}" >/dev/null 2>&1 || true
	rm -f "${output}"
}
trap cleanup EXIT

echo "==> [1/4] building inputs image (network allowed, digest-verified, cached)"
docker build -t "${inputs_image}" \
	-f "${here}/Containerfile.inputs" "${here}"

echo
echo "==> [2/4] building verification image with --network=none"
docker build --network=none -t "${shell_image}" \
	-f "${here}/Containerfile" "${here}"

echo
echo "==> [3/4] running weston (headless, shell=wlvision-shell-probe) and the client"
docker_rc=0
docker run --rm -i --name "${container_name}" \
	"${shell_image}" /bin/sh -s >"${output}" 2>&1 <<'INNER' || docker_rc=$?
set -u
export XDG_RUNTIME_DIR="$(mktemp -d /tmp/wlvision-runtime.XXXXXX)"
chmod 700 "${XDG_RUNTIME_DIR}"
export WAYLAND_DISPLAY=wlvision-test
weston_log=/tmp/weston.log

# The fake seat is the headless backend's seat (pointer + keyboard); the
# shell needs it for keyboard focus.  --use-pixman matches the image, which
# is built without the GL renderer.
/usr/bin/weston \
	--backend=headless-backend.so \
	--use-pixman \
	--fake-seat \
	--width=1024 \
	--height=640 \
	--socket=wlvision-test \
	--shell=wlvision-shell-probe \
	>"${weston_log}" 2>&1 &
wpid=$!

socket=no
i=0
while [ "${i}" -lt 200 ]; do
	if [ -S "${XDG_RUNTIME_DIR}/${WAYLAND_DISPLAY}" ]; then
		socket=yes
		break
	fi
	if ! kill -0 "${wpid}" 2>/dev/null; then
		break
	fi
	sleep 0.05
	i=$((i + 1))
done

client_rc=99
if [ "${socket}" = yes ]; then
	/usr/local/bin/wlvision-shell-client
	client_rc=$?
	# Give weston time to see the client disconnect (surface_removed).
	sleep 0.5
fi

# Phase 5 fixture stage: each mode runs against the same Weston instance; its
# own stdout lines are printed between markers so the outer assertions can
# section them per mode.
fixture_anim_rc=99
fixture_ignore_rc=99
fixture_delayed_rc=99
fixture_input_rc=99
fixture_quiet_rc=99
fixture_exit_rc=99
if [ "${socket}" = yes ]; then
	echo "wlvision-inner: --- fixture anim ---"
	/usr/local/bin/wlvision-fixture --title wlvision-fixture-anim \
		--frames 4 --animate --period 0 --exit-after 3000
	fixture_anim_rc=$?
	echo "wlvision-inner: fixture anim rc=${fixture_anim_rc}"

	echo "wlvision-inner: --- fixture ignore ---"
	/usr/local/bin/wlvision-fixture --title wlvision-fixture-ignore \
		--frames 1 --resize ignore --exit-after 1500
	fixture_ignore_rc=$?
	echo "wlvision-inner: fixture ignore rc=${fixture_ignore_rc}"

	echo "wlvision-inner: --- fixture delayed ---"
	/usr/local/bin/wlvision-fixture --title wlvision-fixture-delayed \
		--resize delayed --resize-delay 300 --exit-after 1500
	fixture_delayed_rc=$?
	echo "wlvision-inner: fixture delayed rc=${fixture_delayed_rc}"

	echo "wlvision-inner: --- fixture input ---"
	/usr/local/bin/wlvision-fixture --title wlvision-fixture-input \
		--frames 1 --report-input --resize none --exit-after 1000
	fixture_input_rc=$?
	echo "wlvision-inner: fixture input rc=${fixture_input_rc}"

	echo "wlvision-inner: --- fixture quiet ---"
	/usr/local/bin/wlvision-fixture --title wlvision-fixture-quiet \
		--frames 1 --resize none --exit-after 1000
	fixture_quiet_rc=$?
	echo "wlvision-inner: fixture quiet rc=${fixture_quiet_rc}"

	echo "wlvision-inner: --- fixture exit ---"
	/usr/local/bin/wlvision-fixture --title wlvision-fixture-exit \
		--resize exit --exit-after 1500
	fixture_exit_rc=$?
	echo "wlvision-inner: fixture exit rc=${fixture_exit_rc}"
fi

kill "${wpid}" 2>/dev/null || true
wait "${wpid}" 2>/dev/null || true

echo "wlvision-inner: socket=${socket} client_rc=${client_rc}"
echo "wlvision-inner: fixture rcs anim=${fixture_anim_rc} ignore=${fixture_ignore_rc} delayed=${fixture_delayed_rc} input=${fixture_input_rc} quiet=${fixture_quiet_rc} exit=${fixture_exit_rc}"
echo "wlvision-inner: --- weston log (stdout+stderr) ---"
cat "${weston_log}"
exit 0
INNER

echo
echo "==> [4/4] asserting on the shell report"

report="$(grep '^wlvision-shell: ' "${output}" || true)"
client_report="$(grep '^wlvision-client: ' "${output}" || true)"
log_contents="$(cat "${output}")"

fail=0

check() {
	local text="$1"
	local pattern="$2"
	local description="$3"

	if printf '%s\n' "${text}" | grep -Eq "${pattern}"; then
		echo "ok   - ${description}"
	else
		echo "FAIL - ${description}"
		echo "       pattern: ${pattern}"
		fail=1
	fi
}

check "${log_contents}" 'wlvision-inner: socket=yes client_rc=0' \
	'weston started and the client exited 0'
check "${report}" \
	'^wlvision-shell: added handle=app-[0-9]+ title=wlvision-probe-title app_id=wlvision\.probe geometry=' \
	'toplevel added with the expected title, app id and app-N handle'
check "${report}" \
	'^wlvision-shell: activated handle=app-[0-9]+ seat=default keyboard=yes keyboard_focus=yes' \
	'first toplevel activated and given keyboard focus through the seat'
check "${report}" \
	'^wlvision-shell: configured handle=app-[0-9]+ request_id=[0-9]+ requested=640x480' \
	'resize configure requested through weston_desktop_surface_set_size'
check "${report}" \
	'^wlvision-shell: committed handle=app-[0-9]+ .* buffer=640x480 .*matching=yes' \
	'client committed a buffer matching the 640x480 resize request'
check "${report}" \
	'^wlvision-shell: removed handle=app-[0-9]+ title=wlvision-probe-title app_id=wlvision\.probe' \
	'removing the client produced a removal line'
check "${client_report}" \
	'^wlvision-client: configure #2 width=640 height=480 activated=yes' \
	'client saw the second (resize) configure with the activated state'
check "${client_report}" \
	'^wlvision-client: committed buffer=640x480' \
	'client committed the matching buffer'

# --- Phase 5 fixture stage -----------------------------------------------

# The fixture lines are printed by the inner session between markers; split
# them per mode so each expected line is asserted against its own run.
fixture_section() {
	local name="$1"

	awk -v marker="wlvision-inner: --- fixture ${name} ---" '
		$0 == marker { inside = 1; next }
		inside && /^wlvision-inner: --- fixture / { inside = 0 }
		inside
	' "${output}"
}

fixture_anim="$(fixture_section anim)"
fixture_ignore="$(fixture_section ignore)"
fixture_delayed="$(fixture_section delayed)"
fixture_input="$(fixture_section input)"
fixture_quiet="$(fixture_section quiet)"
fixture_exit="$(fixture_section exit)"

check "${log_contents}" \
	'wlvision-inner: fixture rcs anim=0 ignore=0 delayed=0 input=0 quiet=0 exit=0' \
	'every fixture mode exited 0'

check "${fixture_anim}" '^wlvision-fixture: connected' \
	'fixture connected to the compositor'
check "${fixture_anim}" \
	'^wlvision-fixture: configure n=1 width=320 height=240 activated=no' \
	'fixture saw the first configure and mapped a matching buffer'
check "${fixture_anim}" \
	'^wlvision-fixture: done frames=4 configures=2' \
	'animation burst committed its 4-frame budget before the deadline'
anim_commits="$(printf '%s\n' "${fixture_anim}" |
	grep -c '^wlvision-fixture: committed width=.* frame=' || true)"
anim_distinct="$(printf '%s\n' "${fixture_anim}" |
	sed -n 's/^wlvision-fixture: committed width=.* frame=\([0-9]*\)$/\1/p' |
	sort -u | grep -c . || true)"
if [ "${anim_commits}" = 4 ] && [ "${anim_distinct}" = 4 ]; then
	echo "ok   - animation burst produced exactly 4 distinct committed frames"
else
	echo "FAIL - animation burst produced exactly 4 distinct committed frames"
	echo "       commits=${anim_commits} distinct=${anim_distinct}"
	fail=1
fi

check "${fixture_ignore}" \
	'^wlvision-fixture: configure n=2 width=640 height=480 activated=yes' \
	'ignored-resize fixture saw the resize configure'
check "${fixture_ignore}" \
	'^wlvision-fixture: committed width=320 height=240 frame=1' \
	'ignored-resize fixture kept its original buffer'
check "${fixture_ignore}" '^wlvision-fixture: done frames=1 configures=2' \
	'ignored-resize fixture shut down normally without committing the resize'
if printf '%s\n' "${fixture_ignore}" |
	grep -Eq '^wlvision-fixture: committed width=640 height=480'; then
	echo "FAIL - ignored-resize fixture committed a matching buffer anyway"
	fail=1
else
	echo "ok   - ignored-resize fixture never committed a matching buffer"
fi

check "${fixture_delayed}" \
	'^wlvision-fixture: configure n=2 width=640 height=480 activated=yes' \
	'delayed-resize fixture saw the resize configure'
check "${fixture_delayed}" \
	'^wlvision-fixture: committed width=640 height=480 frame=2' \
	'delayed-resize fixture committed the matching buffer after the delay'
check "${fixture_delayed}" '^wlvision-fixture: done frames=2 configures=2' \
	'delayed-resize fixture shut down normally'

check "${fixture_input}" '^wlvision-fixture: connected' \
	'input-reporting fixture connected and bound the seat'
check "${fixture_input}" '^wlvision-fixture: done frames=2 configures=2' \
	'input-reporting fixture shut down normally'

check "${fixture_quiet}" '^wlvision-fixture: committed width=320 height=240 frame=1' \
	'quiet fixture mapped its window once'
check "${fixture_quiet}" '^wlvision-fixture: done frames=2 configures=2' \
	'quiet fixture stayed alive without animation and shut down normally'

check "${fixture_exit}" \
	'^wlvision-fixture: configure n=2 width=640 height=480 activated=yes' \
	'exit-on-resize fixture saw the resize configure'
check "${fixture_exit}" '^wlvision-fixture: done frames=1 configures=2' \
	'exit-on-resize fixture destroyed its window during the resize and exited 0'
if printf '%s\n' "${fixture_exit}" |
	grep -Eq '^wlvision-fixture: committed width=640 height=480'; then
	echo "FAIL - exit-on-resize fixture committed the resize it was meant to abandon"
	fail=1
else
	echo "ok   - exit-on-resize fixture abandoned the resize"
fi
added_handles="$(printf '%s\n' "${report}" | sed -n 's/^wlvision-shell: added handle=\(app-[0-9]*\) .*/\1/p')"
removed_handles="$(printf '%s\n' "${report}" | sed -n 's/^wlvision-shell: removed handle=\(app-[0-9]*\) .*/\1/p')"
added_count="$(printf '%s\n' "${added_handles}" | grep -c 'app-' || true)"
unique_count="$(printf '%s\n' "${added_handles}" | sort -u | grep -c 'app-' || true)"

if [ -n "${added_handles}" ] &&
   [ "${added_count}" = "${unique_count}" ] &&
   [ "${added_handles}" = "${removed_handles}" ]; then
	echo "ok   - handles are unique and every added handle was removed"
else
	echo "FAIL - handles are unique and every added handle was removed"
	echo "       added:   ${added_handles:-<none>}"
	echo "       removed: ${removed_handles:-<none>}"
	fail=1
fi

if [ "${fail}" -ne 0 ] || [ "${docker_rc}" -ne 0 ]; then
	echo
	echo "FAILED (docker_rc=${docker_rc})"
	echo "---- wlvision-shell report ----"
	printf '%s\n' "${report:-<no report lines>}"
	echo "---- weston log (stdout+stderr) ----"
	cat "${output}"
	exit 1
fi

echo
echo "PASSED"
echo "---- wlvision-shell report ----"
printf '%s\n' "${report}"
echo "---- wlvision-client report ----"
printf '%s\n' "${client_report}"
exit 0
