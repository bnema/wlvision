// Package integration: the isolation matrix gate.
//
// TestIsolationMatrix proves, against a real rootless engine, that one live
// session enforces every boundary the design claims: host paths are
// unreachable, the network is absent, no engine or host socket is exposed, no
// device beyond the engine's minimal defaults exists, the image root is
// read-only and only bounded memory-backed mounts are writable, the two
// identities are separated, the engine configuration the container was created
// with matches the adapter's fixed policy, the kernel reports the confinement,
// and the CLI refuses to store an artifact outside the session's export
// directory.
//
// It reuses the harness in lifecycle_test.go (newHarness, cliArgs, run,
// succeed, docker, inContainer, requireInContainer) and the container layout
// constants it pins, so this file checks the deployed reality rather than a
// second copy of the design. Every probe is one `sh -c` invocation with an
// explicit exit status, so a failure names the check that failed.
//
// The control protocol's credential refusal of the private Wayland global is
// proven by the module gate in test/module-runtime: it runs with a
// world-accessible runtime directory and shows a non-control UID is refused
// the bind. It is deliberately not repeated here; asserting file modes against
// the session's own 0700 directory would be a weaker proof of the same claim.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/session"
)

// Explicit limits the session is created with, so the engine inspection can
// assert them instead of the engine's defaults.
const (
	isoMemoryLimit = int64(1 << 30)
	isoPidsLimit   = 512
)

// isoTmpfsExpect is one of the three mounts the session declares.
type isoTmpfsExpect struct {
	path  string
	bytes int64
	exec  bool
}

// isoDeclaredTmpfs is the mount set from session.sessionMounts, repeated here
// so the gate checks the deployed mounts rather than the constant it is
// verifying.
var isoDeclaredTmpfs = []isoTmpfsExpect{
	{runtimeDir, 64 << 20, true},
	{"/tmp", 32 << 20, false},
	{"/home/agent", 32 << 20, false},
}

// isoHostBacked lists the filesystem types that imply a host path is mounted.
// A read-write mount of one of these inside the session is a leak.
var isoHostBacked = map[string]bool{
	"overlay": true, "ext4": true, "ext3": true, "ext2": true, "xfs": true,
	"btrfs": true, "f2fs": true, "zfs": true, "nfs": true, "nfs4": true,
	"cifs": true, "9p": true, "virtiofs": true,
}

// isoDeviceAllowlist is the engine's minimal default device set, read from the
// running session. Anything else is an unexpected device.
var isoDeviceAllowlist = map[string]bool{
	"console": true, "full": true, "null": true, "random": true,
	"tty": true, "urandom": true, "zero": true,
}

// TestIsolationMatrix is the Phase 6 gate. It skips unless the harness
// environment variables are set, exactly like the other integration gates.
func TestIsolationMatrix(t *testing.T) {
	h := newHarness(t)

	// One live session, created with explicit limits and waited for, is the
	// subject of every subtest.
	h.succeed("session", "create", "--session", h.session,
		"--memory", strconv.FormatInt(isoMemoryLimit, 10),
		"--pids", strconv.Itoa(isoPidsLimit), "--wait")

	// Host sentinels: one in a temporary host directory outside the state
	// root, one inside the state root but outside the session's own
	// directories. The container never sees either path.
	const sentinelContent = "wlvision-host-sentinel"
	hostDir, err := os.MkdirTemp("", "wlvision-isolation-host-*")
	if err != nil {
		t.Fatalf("create the host sentinel directory: %v", err)
	}
	hostSentinel := filepath.Join(hostDir, "host-sentinel")
	stateSentinel := filepath.Join(h.stateRoot, "isolation-sentinel-"+h.session)

	for _, sentinel := range []string{hostSentinel, stateSentinel} {
		if err := os.WriteFile(sentinel, []byte(sentinelContent), 0o600); err != nil {
			t.Fatalf("write the sentinel %s: %v", sentinel, err)
		}
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(hostDir)
		_ = os.Remove(stateSentinel)
	})

	t.Run("host sentinels are unreachable and untampered", func(t *testing.T) {
		for _, probe := range []struct{ name, path, dir, base string }{
			{"outside the state root", hostSentinel, hostDir, filepath.Base(hostSentinel)},
			{"inside the state root", stateSentinel, h.stateRoot, filepath.Base(stateSentinel)},
		} {
			for _, uid := range []uint32{controlUID, applicationUID} {
				t.Run(fmt.Sprintf("%s as %d", probe.name, uid), func(t *testing.T) {
					// A path that does not exist in the container's own root
					// is the assertion; /proc/self/root resolves to that root,
					// not to the host's.
					script := fmt.Sprintf(`p=%s; d=%s; n=%s
test ! -e "$p" || exit 10
test ! -r "$p" || exit 11
test ! -e "/proc/self/root$p" || exit 12
! ls -a "$d" 2>/dev/null | grep -qx "$n" || exit 13
if cat "$p" 2>/dev/null; then exit 14; fi
if printf tampered > "$p" 2>/dev/null; then exit 15; fi
exit 0
`, isoQuote(probe.path), isoQuote(probe.dir), isoQuote(probe.base))

					if output, code := h.isoProbe(uid, script); code != 0 {
						t.Errorf("the sentinel %s was reachable as %d (exit %d): %s", probe.path, uid, code, output)
					}
				})
			}

			// Byte-identical afterwards, on the host.
			content, err := os.ReadFile(probe.path)
			if err != nil {
				t.Errorf("the sentinel %s disappeared: %v", probe.path, err)
				continue
			}
			if string(content) != sentinelContent {
				t.Errorf("the sentinel %s changed to %q", probe.path, content)
			}
		}
	})

	t.Run("the network is absent", func(t *testing.T) {
		if interfaces := strings.TrimSpace(h.isoRequire(controlUID, "ls /sys/class/net")); interfaces != "lo" {
			t.Errorf("the session has interfaces %q, want only lo", interfaces)
		}

		route := h.isoRequire(controlUID, "cat /proc/net/route")
		if strings.Contains(route, "00000000") {
			t.Errorf("the session has a default route:\n%s", route)
		}

		for _, uid := range []uint32{controlUID, applicationUID} {
			// A bounded connect attempt to a routable address must fail. The
			// timeout bounds it; the reason must be a network refusal, not a
			// missing probe (the image's /bin/sh is bash and supports /dev/tcp,
			// which is checked by requiring one of the refusal messages).
			output, code := h.isoProbe(uid, "timeout 3 sh -c 'exec 3<>/dev/tcp/1.1.1.1/443'")
			if code == 0 {
				t.Errorf("a connect out of the session succeeded as %d", uid)
				continue
			}
			reason := strings.ToLower(output)
			if !strings.Contains(reason, "unreachable") && !strings.Contains(reason, "refused") && !strings.Contains(reason, "timed out") {
				t.Errorf("the connect probe as %d failed for an unexpected reason (exit %d): %s", uid, code, output)
			}
		}
	})

	t.Run("no engine, host, or agent sockets are exposed", func(t *testing.T) {
		script := `for p in /run/docker.sock /var/run/docker.sock \
     /run/containerd/containerd.sock /var/run/containerd/containerd.sock \
     /run/dbus/system_bus_socket /var/run/dbus/system_bus_socket \
     /run/user/0 /run/user/1000; do
  test ! -e "$p" || exit 10
done
test -z "${SSH_AUTH_SOCK:-}" || exit 11
test -z "${GPG_AGENT_INFO:-}" || exit 12
test -z "${DBUS_SYSTEM_BUS_ADDRESS:-}" || exit 13
test -z "${DBUS_SESSION_BUS_ADDRESS:-}" || exit 14
test ! -S "$HOME/.gnupg/S.gpg-agent" || exit 15
test ! -S "/run/user/$(id -u)/gnupg/S.gpg-agent" || exit 16
ls -A /run/user 2>/dev/null | grep -q . && exit 17
exit 0
`
		for _, uid := range []uint32{controlUID, applicationUID} {
			if output, code := h.isoProbe(uid, script); code != 0 {
				t.Errorf("a host socket is reachable as %d (exit %d): %s", uid, code, output)
			}
		}

		// No host runtime directory is mounted: the only mount under /run is
		// the session's own runtime tmpfs.
		for point, mount := range h.isoMounts() {
			if point == "/run" || point == "/run/user" || point == "/run/dbus" ||
				(strings.HasPrefix(point, "/run/") && point != runtimeDir) {
				t.Errorf("the mount %s (%s) exposes a host runtime directory", point, mount.fstype)
			}
		}
	})

	t.Run("only the engine's minimal devices exist", func(t *testing.T) {
		for _, path := range []string{"/dev/kvm", "/dev/dri", "/dev/disk", "/dev/mapper"} {
			if output, code := h.isoProbe(controlUID, "test ! -e "+path); code != 0 {
				t.Errorf("%s is present in the session: %s", path, output)
			}
		}

		// Only real device nodes count: /dev/core, /dev/ptmx and the stdio
		// symlinks point at devices but are engine defaults, not extra nodes.
		blocks := strings.TrimSpace(h.isoRequire(controlUID, `for p in /dev/*; do [ -L "$p" ] && continue; [ -b "$p" ] && echo "$p"; done; exit 0`))
		if blocks != "" {
			t.Errorf("the session has block devices:\n%s", blocks)
		}

		characters := strings.Fields(h.isoRequire(controlUID, `for p in /dev/*; do [ -L "$p" ] && continue; [ -c "$p" ] && echo "${p#/dev/}"; done; exit 0`))
		for _, name := range characters {
			if !isoDeviceAllowlist[name] {
				t.Errorf("the session has an unexpected device /dev/%s", name)
			}
		}

		for _, uid := range []uint32{controlUID, applicationUID} {
			// Creating a device needs CAP_MKNOD, which the container drops.
			script := `if mknod /tmp/iso-device c 1 3 2>/dev/null; then rm -f /tmp/iso-device; exit 9; fi; exit 0`
			if output, code := h.isoProbe(uid, script); code != 0 {
				t.Errorf("the identity %d created a device node: %s", uid, output)
			}
		}
	})

	t.Run("mounts are bounded and the root is read-only", func(t *testing.T) {
		mounts := h.isoMounts()

		root, ok := mounts["/"]
		if !ok {
			t.Fatalf("the session reports no root mount: %v", mounts)
		}
		if !isoHasOption(root.options, "ro") {
			t.Errorf("the image root is writable: %s", root.options)
		}

		for _, expect := range isoDeclaredTmpfs {
			mount, ok := mounts[expect.path]
			if !ok {
				t.Errorf("the declared tmpfs %s is not mounted", expect.path)
				continue
			}
			if !isoHasOption(mount.options, "rw") {
				t.Errorf("%s is not writable: %s", expect.path, mount.options)
			}
			size, ok := isoTmpfsSize(mount.options)
			if !ok || size != expect.bytes {
				t.Errorf("%s has size %d (parsed=%v), want %d: %s", expect.path, size, ok, expect.bytes, mount.options)
			}
			for _, want := range []string{"nosuid", "nodev"} {
				if !isoHasOption(mount.options, want) {
					t.Errorf("%s lacks %s: %s", expect.path, want, mount.options)
				}
			}
			if expect.exec && isoHasOption(mount.options, "noexec") {
				t.Errorf("%s denies execution but holds injected payloads: %s", expect.path, mount.options)
			}
			if !expect.exec && !isoHasOption(mount.options, "noexec") {
				t.Errorf("%s allows execution: %s", expect.path, mount.options)
			}
			mode := strings.TrimSpace(h.isoRequire(controlUID, "stat -c %a "+expect.path))
			if mode != "1777" {
				t.Errorf("%s has mode %s, want 1777", expect.path, mode)
			}
		}

		// The only writable mounts are memory-backed: no read-write mount is a
		// host filesystem, and the only writable tmpfs besides the engine's own
		// /dev and /dev/shm are the declared ones.
		declared := map[string]bool{runtimeDir: true, "/tmp": true, "/home/agent": true}
		for point, mount := range mounts {
			if !isoHasOption(mount.options, "rw") {
				continue
			}
			if isoHostBacked[mount.fstype] {
				t.Errorf("the session has a writable host-backed mount %s (%s)", point, mount.fstype)
			}
			if mount.fstype == "tmpfs" && point != "/dev" && point != "/dev/shm" && !declared[point] {
				t.Errorf("the session has an undeclared writable tmpfs %s", point)
			}
		}

		// Neither identity can write outside the declared mounts.
		for _, uid := range []uint32{controlUID, applicationUID} {
			script := `if touch /usr/libexec/iso-probe 2>/dev/null; then rm -f /usr/libexec/iso-probe; exit 9; fi
if mkdir /etc/iso-probe 2>/dev/null; then rmdir /etc/iso-probe; exit 10; fi
exit 0`
			if output, code := h.isoProbe(uid, script); code != 0 {
				t.Errorf("the identity %d wrote to the image root: %s", uid, output)
			}
		}
	})

	t.Run("the identities and the control plane are separated", func(t *testing.T) {
		for _, uid := range []uint32{controlUID, applicationUID} {
			got := strings.TrimSpace(h.isoRequire(uid, "id -u"))
			if got != strconv.Itoa(int(uid)) {
				t.Errorf("the identity running the probe reports uid %s, want %d", got, uid)
			}
		}

		envelope := h.succeed("doctor")
		var report session.DoctorReport
		if err := json.Unmarshal(envelope.Result, &report); err != nil {
			t.Fatalf("the doctor report is unreadable: %v", err)
		}
		if report.ControlUID != controlUID || report.AppUID != applicationUID {
			t.Errorf("doctor reports control=%d application=%d, want %d and %d", report.ControlUID, report.AppUID, controlUID, applicationUID)
		}

		// The control directory is private to the control identity. Note that a
		// bare `stat` of the directory can succeed because POSIX resolves it
		// through the world-executable parent; the boundary is that the first
		// path component inside it cannot be reached at all.
		controlDirChecks := []string{
			"ls " + controlDir,
			"cat " + controlDir + "/status.json",
			"stat " + controlDir + "/status.json",
			"test -r " + controlDir,
			"test -x " + controlDir,
			"test -S " + controlDir + "/agent.sock",
		}
		for _, check := range controlDirChecks {
			if output, code := h.isoProbe(applicationUID, check); code == 0 {
				t.Errorf("the application identity could run %q against the control directory: %s", check, output)
			}
		}
		if output, code := h.inContainer(applicationUID, callBinary, "snapshot"); code == 0 {
			t.Errorf("the application identity reached the controller: %s", output)
		}

		// The control identity is a positive control: the directory really is
		// readable and the controller really is reachable for it, so the
		// checks above are not vacuous.
		h.isoRequire(controlUID, "test -r "+controlDir)
		h.isoRequire(controlUID, "test -x "+controlDir)
		h.isoRequire(controlUID, "test -w "+exportDir)

		// The application identity cannot write anywhere under the export
		// directory, which is where captures are retrieved from. The first two
		// are positive assertions that the directory really is closed to it;
		// the rest must fail outright.
		h.isoRequire(applicationUID, "test ! -w "+exportDir)
		h.isoRequire(applicationUID, "test ! -x "+exportDir)
		for _, check := range []string{
			"ls " + exportDir,
			"touch " + exportDir + "/iso-probe",
			"mkdir " + exportDir + "/iso-probe",
		} {
			if output, code := h.isoProbe(applicationUID, check); code == 0 {
				t.Errorf("the application identity could run %q against the export directory: %s", check, output)
			}
		}
	})

	t.Run("the engine configuration matches the adapter's policy", func(t *testing.T) {
		inspect := h.isoInspect()

		if inspect.Config.User != fmt.Sprintf("%d:%d", controlUID, controlUID) {
			t.Errorf("the container runs as %q, want the control identity", inspect.Config.User)
		}
		if !inspect.HostConfig.ReadonlyRootfs {
			t.Error("the container does not run with a read-only root")
		}
		if inspect.HostConfig.Privileged {
			t.Error("the container is privileged")
		}
		if inspect.HostConfig.NetworkMode != "none" {
			t.Errorf("the container network mode is %q, want none", inspect.HostConfig.NetworkMode)
		}
		if len(inspect.HostConfig.CapAdd) != 0 {
			t.Errorf("the container adds capabilities: %v", inspect.HostConfig.CapAdd)
		}
		if !isoContains(inspect.HostConfig.CapDrop, "ALL") {
			t.Errorf("the container does not drop all capabilities: %v", inspect.HostConfig.CapDrop)
		}
		if !isoContains(inspect.HostConfig.SecurityOpt, "no-new-privileges") {
			t.Errorf("the container does not set no-new-privileges: %v", inspect.HostConfig.SecurityOpt)
		}
		// The built-in seccomp profile is the engine default; the adapter must
		// never disable it. That it is active is proven from the kernel state
		// in the next subtest (Seccomp mode 2).
		for _, option := range inspect.HostConfig.SecurityOpt {
			if strings.Contains(option, "seccomp=unconfined") {
				t.Errorf("the container disables seccomp: %v", inspect.HostConfig.SecurityOpt)
			}
		}
		if len(inspect.HostConfig.Binds) != 0 {
			t.Errorf("the container has bind mounts: %v", inspect.HostConfig.Binds)
		}
		for _, mount := range inspect.Mounts {
			if mount.Type != "tmpfs" {
				t.Errorf("the container has a %s mount, and only tmpfs is allowed", mount.Type)
			}
		}
		for _, expect := range isoDeclaredTmpfs {
			got, ok := inspect.HostConfig.Tmpfs[expect.path]
			if !ok {
				t.Errorf("the container declares no tmpfs at %s", expect.path)
				continue
			}
			if want := isoTmpfsOptions(expect.bytes, expect.exec); got != want {
				t.Errorf("the tmpfs %s is %q, want %q", expect.path, got, want)
			}
		}
		if inspect.HostConfig.Memory != isoMemoryLimit {
			t.Errorf("the container memory limit is %d, want %d", inspect.HostConfig.Memory, isoMemoryLimit)
		}
		if inspect.HostConfig.PidsLimit != isoPidsLimit {
			t.Errorf("the container pids limit is %d, want %d", inspect.HostConfig.PidsLimit, isoPidsLimit)
		}

		// A limit the engine cannot enforce is reported as a degradation by
		// doctor, never claimed as enforced.
		envelope := h.succeed("doctor")
		var report session.DoctorReport
		if err := json.Unmarshal(envelope.Result, &report); err != nil {
			t.Fatalf("the doctor report is unreadable: %v", err)
		}
		if !report.Capabilities.Rootless {
			t.Error("doctor does not report the engine as rootless")
		}
		if report.Capabilities.SeccompProfile == "" {
			t.Error("doctor reports no seccomp profile")
		}
		if report.Capabilities.MemoryLimit == isoContains(report.Degradations, engine.DegradationMemoryLimit) {
			t.Errorf("doctor's memory limit claim (%v) contradicts its degradations %v", report.Capabilities.MemoryLimit, report.Degradations)
		}
		if report.Capabilities.PidsLimit == isoContains(report.Degradations, engine.DegradationPidsLimit) {
			t.Errorf("doctor's pids limit claim (%v) contradicts its degradations %v", report.Capabilities.PidsLimit, report.Degradations)
		}
		if report.Capabilities.CgroupVersion != "" && report.Capabilities.CgroupVersion != "2" &&
			!isoContains(report.Degradations, engine.DegradationCgroupV1) {
			t.Errorf("doctor does not report cgroup v1 (version %q): %v", report.Capabilities.CgroupVersion, report.Degradations)
		}
	})

	t.Run("the kernel reports the confinement", func(t *testing.T) {
		for _, uid := range []uint32{controlUID, applicationUID} {
			status := isoStatusFields(h.isoRequire(uid, "cat /proc/self/status"))
			if got := status["Seccomp"]; got != "2" {
				t.Errorf("seccomp mode is %q as %d, want 2 (filtered)", got, uid)
			}
			if got := status["NoNewPrivs"]; got != "1" {
				t.Errorf("NoNewPrivs is %q as %d, want 1", got, uid)
			}
			for _, field := range []string{"CapEff", "CapBnd", "CapPrm", "CapInh", "CapAmb"} {
				if got, ok := status[field]; ok && strings.TrimLeft(got, "0") != "" {
					t.Errorf("%s is %q as %d, want empty", field, got, uid)
				}
			}
		}
	})

	t.Run("the export root confines the CLI", func(t *testing.T) {
		exportRoot := filepath.Join(h.stateRoot, "export", h.session)
		outside, err := os.MkdirTemp("", "wlvision-isolation-outside-*")
		if err != nil {
			t.Fatalf("create the outside directory: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(outside) })

		if err := os.MkdirAll(exportRoot, 0o700); err != nil {
			t.Fatalf("create the session export directory: %v", err)
		}
		link := filepath.Join(exportRoot, "linked")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("create the escaping symlink: %v", err)
		}

		// A capture needs a live frame; the symlinked-directory refusal happens
		// while the artifact is stored, so drive the session first.
		h.startFixtureClient()
		h.waitForWindow()

		for _, probe := range []struct {
			name   string
			output string
		}{
			{"absolute name", "/etc/wlvision-isolation"},
			{"traversing name", "../wlvision-isolation"},
			{"name through a symlinked directory", "linked/wlvision-isolation"},
		} {
			t.Run(probe.name, func(t *testing.T) {
				stdout, stderr, code := h.run("screenshot", "--session", h.session, "--output", probe.output)
				if code != 2 {
					t.Errorf("screenshot --output %q exited %d, want 2 (usage); stdout: %s stderr: %s", probe.output, code, stdout, stderr)
					return
				}
				if got := isoFailureCode(t, stdout); got != "usage_error" {
					t.Errorf("screenshot --output %q reported %q, want usage_error (%s)", probe.output, got, stdout)
				}
			})
		}

		// Nothing was written outside the session's export directory. The
		// symlink target is the strongest witness: if the CLI had followed it,
		// the artifact would be in there.
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatalf("read the outside directory: %v", err)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			t.Errorf("the CLI wrote outside the session export directory: %v", names)
		}
	})
}

// isoProbe runs one shell script in the session as uid.
func (h *harness) isoProbe(uid uint32, script string) (string, int) {
	h.t.Helper()
	return h.inContainer(uid, "sh", "-c", script)
}

// isoRequire runs one shell script and requires it to succeed.
func (h *harness) isoRequire(uid uint32, script string) string {
	h.t.Helper()
	output, code := h.isoProbe(uid, script)
	if code != 0 {
		h.t.Fatalf("in container as %d: %q exited %d: %s", uid, script, code, output)
	}
	return output
}

// isoInspect returns the engine's view of the session container.
func (h *harness) isoInspect() isoContainerInspect {
	h.t.Helper()

	output, code := h.docker("inspect", h.container)
	if code != 0 {
		h.t.Fatalf("docker inspect %s exited %d: %s", h.container, code, output)
	}
	var documents []isoContainerInspect
	if err := json.Unmarshal([]byte(output), &documents); err != nil {
		h.t.Fatalf("the engine inspection is unreadable: %v", err)
	}
	if len(documents) != 1 {
		h.t.Fatalf("the engine reported %d documents for %s", len(documents), h.container)
	}
	return documents[0]
}

// isoContainerInspect is the subset of `docker inspect` this gate asserts on.
type isoContainerInspect struct {
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
	HostConfig struct {
		Binds          []string          `json:"Binds"`
		NetworkMode    string            `json:"NetworkMode"`
		CapAdd         []string          `json:"CapAdd"`
		CapDrop        []string          `json:"CapDrop"`
		Privileged     bool              `json:"Privileged"`
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		Memory         int64             `json:"Memory"`
		PidsLimit      int64             `json:"PidsLimit"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type string `json:"Type"`
	} `json:"Mounts"`
}

// isoMount is one line of /proc/mounts as the session sees it.
type isoMount struct {
	point   string
	fstype  string
	options string
}

// isoMounts reads the session's mount table, keyed by mount point.
func (h *harness) isoMounts() map[string]isoMount {
	h.t.Helper()

	output := h.isoRequire(controlUID, "cat /proc/mounts")
	mounts := make(map[string]isoMount)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		point := isoUnescapeMount(fields[1])
		mounts[point] = isoMount{point: point, fstype: fields[2], options: fields[3]}
	}
	return mounts
}

// isoUnescapeMount decodes the octal escapes /proc/mounts uses for spaces and
// friends in a mount point.
func isoUnescapeMount(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}

// isoHasOption reports whether one comma-separated mount option is present.
func isoHasOption(options, want string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

// isoTmpfsSize reads the byte size from a mount's options.
func isoTmpfsSize(options string) (int64, bool) {
	for _, option := range strings.Split(options, ",") {
		if !strings.HasPrefix(option, "size=") {
			continue
		}
		text := strings.TrimPrefix(option, "size=")
		multiplier := int64(1)
		if len(text) > 0 {
			switch text[len(text)-1] {
			case 'k', 'K':
				multiplier, text = 1<<10, text[:len(text)-1]
			case 'm', 'M':
				multiplier, text = 1<<20, text[:len(text)-1]
			case 'g', 'G':
				multiplier, text = 1<<30, text[:len(text)-1]
			}
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, false
		}
		return value * multiplier, true
	}
	return 0, false
}

// isoTmpfsOptions renders the options the adapter passes for one tmpfs, so the
// inspection is compared with the adapter's own wording.
func isoTmpfsOptions(bytes int64, exec bool) string {
	options := []string{"rw", "size=" + strconv.FormatInt(bytes, 10), "mode=01777"}
	if exec {
		options = append(options, "exec")
	} else {
		options = append(options, "noexec")
	}
	return strings.Join(append(options, "nosuid", "nodev"), ",")
}

// isoQuote wraps a value in single quotes for a /bin/sh script.
func isoQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// isoContains reports whether a list holds a value.
func isoContains(list []string, want string) bool {
	for _, value := range list {
		if value == want {
			return true
		}
	}
	return false
}

// isoStatusFields parses the key: value lines of /proc/self/status.
func isoStatusFields(content string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields
}

// isoFailureCode reads the stable code out of one failure document.
func isoFailureCode(t *testing.T, stdout string) string {
	t.Helper()

	var envelope struct {
		Ok    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("the CLI did not answer with a failure document: %v (%q)", err, stdout)
	}
	return envelope.Error.Code
}
