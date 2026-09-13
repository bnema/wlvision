package session

// Container paths shared by the outer CLI and the binaries that run inside a
// session. They are constants rather than configuration because the image and
// the CLI are versioned together: a session that could be pointed at a
// different layout would be a session whose isolation nobody can verify.
const (
	// RuntimeDir is the only writable location of a session. It is a bounded
	// tmpfs mount.
	RuntimeDir = "/run/wlvision"
	// ControlDir is the private control directory. It is mode 0700 and owned
	// by the control UID, so an application cannot open it.
	ControlDir = RuntimeDir + "/control"
	// AgentSocket is the resident controller's socket.
	AgentSocket = ControlDir + "/agent.sock"
	// StatusFile is where the supervisor publishes the state of the session's
	// own processes, used by readiness probes and diagnostics.
	StatusFile = ControlDir + "/status.json"
	// WaylandDir holds the compositor socket. It is shared with the
	// application UID through a dedicated group and nothing else is.
	WaylandDir = RuntimeDir + "/wayland"
	// WaylandDisplay is the compositor socket name inside WaylandDir.
	WaylandDisplay = "wlvision-1"
	// PayloadDir receives injected binaries and bundles.
	PayloadDir = RuntimeDir + "/payload"
	// ExportDir is where the outer CLI may write session artifacts.
	ExportDir = RuntimeDir + "/export"
)

// Container binaries.
const (
	// SupervisorPath is PID 1 of a session.
	SupervisorPath = "/usr/libexec/wlvision-supervisor"
	// AgentPath is the resident controller.
	AgentPath = "/usr/libexec/wlvision-agent"
	// CallPath exchanges one request with the resident controller.
	CallPath = "/usr/libexec/wlvision-call"
)

// The keyboard layout a session guarantees.
//
// The control protocol injects evdev keycodes, so what a key means is decided
// by the keymap the compositor hands to its clients. The supervisor therefore
// pins this RMLVO for the compositor: the same values the keymap artifact the
// CLI resolves text and key names with is generated from. Without it the
// layout would be whatever the host's xkb defaults happen to be.
const (
	// KeyboardRules is the xkb rules set the session uses.
	KeyboardRules = "evdev"
	// KeyboardModel is the xkb model the session uses.
	KeyboardModel = "pc105"
	// KeyboardLayout is the layout V1 guarantees.
	KeyboardLayout = "us"
	// KeyboardVariant and KeyboardOptions are deliberately empty: the layout is
	// the plain US one, with no variant and no options.
	KeyboardVariant = ""
	KeyboardOptions = ""
	// WestonConfigPath is the compositor configuration the supervisor writes.
	// It lives in the control directory, which only the control identity can
	// read, because it is part of the session's privileged state.
	WestonConfigPath = ControlDir + "/weston.ini"
)

// WestonConfig is the compositor configuration of a session, byte for byte.
//
// It is a constant rather than generated text so the layout a session runs with
// is readable in one place, next to the constants the CLI's keymap artifact is
// generated from.
const WestonConfig = `# wlvision session compositor configuration.
#
# The keyboard section pins the layout the session guarantees. The control
# protocol injects evdev keycodes, and a client can only interpret them with the
# keymap the compositor hands out, so the layout must be a property of the
# pinned session image rather than of the host.
[keyboard]
keymap_rules=` + KeyboardRules + `
keymap_model=` + KeyboardModel + `
keymap_layout=` + KeyboardLayout + `
keymap_variant=
keymap_options=
`

// Environment passed to application processes so they reach the compositor
// without learning anything else about the session.
const (
	EnvRuntimeDir     = "XDG_RUNTIME_DIR"
	EnvWaylandDisplay = "WAYLAND_DISPLAY"
	EnvHome           = "HOME"
	// ApplicationHome is the home directory of the application identity. It is a
	// bounded tmpfs the engine provides: an application that writes a profile or
	// a cache needs a home, and it must be one that disappears with the session.
	ApplicationHome = "/home/agent"
)

// ContainerStatus is what the supervisor reports about the session's own
// processes. The service reads it to decide whether a session is ready, and
// reconciles a session whose compositor or controller died.
type ContainerStatus struct {
	Ready   bool   `json:"ready"`
	Weston  string `json:"weston"`
	Agent   string `json:"agent"`
	Message string `json:"message,omitempty"`
}

// Process states reported by ContainerStatus.
const (
	ProcessRunning    = "running"
	ProcessExited     = "exited"
	ProcessNotStarted = "not-started"
)

// PayloadResult is what the in-container receiver prints on success. It is the
// authoritative description of what arrived: the digest is computed while the
// bytes are written, inside the container.
type PayloadResult struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
	Files  int    `json:"files"`
}
