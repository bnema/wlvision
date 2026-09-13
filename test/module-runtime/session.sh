#!/usr/bin/env bash
#
# One wlvision session inside the container: Weston loading the wlvision shell,
# the fixture application, and the control probe.
#
# The control user is 1000 and a second user 1001 exists only to prove that a
# foreign UID cannot bind the control protocol. The runtime directory is
# world-accessible on purpose: the refusal must come from the module's peer
# credential check, not from filesystem permissions.
set -uo pipefail

CONTROL_UID="${CONTROL_UID:-1000}"
FOREIGN_UID="${FOREIGN_UID:-1001}"
RUNTIME_DIR="/run/wlvision-session"

export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export WLVISION_CONTROL_UID="${CONTROL_UID}"

mkdir -p "${RUNTIME_DIR}"
chmod 777 "${RUNTIME_DIR}"

status=0

as_control() {
	setpriv --reuid="${CONTROL_UID}" --regid="${CONTROL_UID}" --clear-groups "$@"
}

as_foreign() {
	setpriv --reuid="${FOREIGN_UID}" --regid="${FOREIGN_UID}" --clear-groups "$@"
}

cleanup() {
	[[ -n "${WESTON_PID:-}" ]] && kill "${WESTON_PID}" 2>/dev/null
	[[ -n "${APP_PID:-}" ]] && kill "${APP_PID}" 2>/dev/null
	wait 2>/dev/null
}
trap cleanup EXIT

echo "==> starting weston with the wlvision shell"
setpriv --reuid="${CONTROL_UID}" --regid="${CONTROL_UID}" --clear-groups \
	weston --backend=headless --renderer=pixman --shell=wlvision-shell \
	--fake-seat --width=800 --height=600 --idle-time=0 \
	> /tmp/weston.log 2>&1 &
WESTON_PID=$!

socket=""
for _ in $(seq 1 150); do
	if ! kill -0 "${WESTON_PID}" 2>/dev/null; then
		echo "weston exited before publishing a socket" >&2
		cat /tmp/weston.log >&2
		exit 1
	fi
	if [[ -S "${RUNTIME_DIR}/wayland-1" ]]; then
		socket="wayland-1"
		break
	fi
	sleep 0.1
done

if [[ -z "${socket}" ]]; then
	echo "timed out waiting for the compositor socket" >&2
	cat /tmp/weston.log >&2
	exit 1
fi

chmod 777 "${RUNTIME_DIR}/wayland-1"
export WAYLAND_DISPLAY="${socket}"
echo "    compositor ready on ${socket}"

echo "==> starting the fixture application"
as_control /usr/local/bin/wlvision-shell-client > /tmp/app.log 2>&1 &
APP_PID=$!

# Wait for the application to map before the probe looks for its window.
for _ in $(seq 1 100); do
	if grep -q "committed buffer=" /tmp/app.log 2>/dev/null; then
		break
	fi
	sleep 0.1
done
grep -m1 "committed buffer=" /tmp/app.log || echo "    (application has not committed yet)"

echo "==> running the control probe as the control user"
as_control /probe
probe_status=$?

echo "==> running the control probe as a foreign user"
as_foreign /probe -expect-denied
denied_status=$?

if [[ ${probe_status} -ne 0 || ${denied_status} -ne 0 ]]; then
	status=1
	echo "--- module report ---" >&2
	grep -a "wlvision-shell:" /tmp/weston.log >&2 || true
	echo "--- weston log tail ---" >&2
	tail -20 /tmp/weston.log >&2
	echo "--- application log ---" >&2
	tail -20 /tmp/app.log >&2
else
	echo "==> module runtime verification passed"
fi

exit "${status}"
