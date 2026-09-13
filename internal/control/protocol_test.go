// Package control defines wlvision's private compositor control protocol and
// the Go facade the resident controller uses to drive it.
//
// The protocol is owned by this project: it is versioned here, generated from
// protocol/wlvision-control.xml, and never relies on private libweston ABI
// crossing an image boundary.
package control

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const protocolPath = "../../protocol/wlvision-control.xml"

// Version 1 exposes exactly these operations and nothing else. The list is the
// contract the CLI is built on: adding an operation means changing this test.
var (
	managerRequests = []string{"create_controller", "destroy"}

	controllerRequests = []string{
		"snapshot",
		"activate",
		"move",
		"resize",
		"close",
		"pointer_motion",
		"pointer_button",
		"pointer_axis",
		"key",
		"authorize_capture",
		"destroy",
	}

	controllerEvents = []string{
		"snapshot",
		"toplevel_changed",
		"toplevel_removed",
		"request_done",
		"request_failed",
		"frame",
		"capture_authorized",
		"resize_configured",
		"resize_done",
	}
)

type protocolXML struct {
	Name       string         `xml:"name,attr"`
	Interfaces []interfaceXML `xml:"interface"`
}

type interfaceXML struct {
	Name     string       `xml:"name,attr"`
	Version  int          `xml:"version,attr"`
	Requests []messageXML `xml:"request"`
	Events   []messageXML `xml:"event"`
	Enums    []enumXML    `xml:"enum"`
}

type messageXML struct {
	Name string   `xml:"name,attr"`
	Type string   `xml:"type,attr"`
	Args []argXML `xml:"arg"`
}

type argXML struct {
	Name      string `xml:"name,attr"`
	Type      string `xml:"type,attr"`
	Interface string `xml:"interface,attr"`
}

type enumXML struct {
	Name    string     `xml:"name,attr"`
	Entries []entryXML `xml:"entry"`
}

type entryXML struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func loadProtocol(t *testing.T) protocolXML {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(protocolPath))
	if err != nil {
		t.Fatalf("read %s: %v", protocolPath, err)
	}

	var protocol protocolXML
	if err := xml.Unmarshal(data, &protocol); err != nil {
		t.Fatalf("parse %s: %v", protocolPath, err)
	}
	return protocol
}

func findInterface(t *testing.T, protocol protocolXML, name string) interfaceXML {
	t.Helper()

	for _, iface := range protocol.Interfaces {
		if iface.Name == name {
			return iface
		}
	}
	t.Fatalf("protocol declares no interface %q (has %v)", name, interfaceNames(protocol))
	return interfaceXML{}
}

func interfaceNames(protocol protocolXML) []string {
	names := make([]string, 0, len(protocol.Interfaces))
	for _, iface := range protocol.Interfaces {
		names = append(names, iface.Name)
	}
	return names
}

func messageNames(messages []messageXML) []string {
	names := make([]string, 0, len(messages))
	for _, message := range messages {
		names = append(names, message.Name)
	}
	return names
}

func findMessage(t *testing.T, messages []messageXML, name string) messageXML {
	t.Helper()

	for _, message := range messages {
		if message.Name == name {
			return message
		}
	}
	t.Fatalf("no message %q (has %v)", name, messageNames(messages))
	return messageXML{}
}

func argNames(message messageXML) []string {
	names := make([]string, 0, len(message.Args))
	for _, arg := range message.Args {
		names = append(names, arg.Name)
	}
	return names
}

func requireArgs(t *testing.T, message messageXML, want []string) {
	t.Helper()

	got := argNames(message)
	if len(got) != len(want) {
		t.Fatalf("%s arguments = %v, want %v", message.Name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s arguments = %v, want %v", message.Name, got, want)
		}
	}
}

// requireArgTypes pins the wire types of a message, not only its argument
// names: a size that crossed as uint where the C module sends int, or a
// revision word that became an int, would still compile on both sides and
// silently change the wire format.
func requireArgTypes(t *testing.T, message messageXML, want []string) {
	t.Helper()

	if len(message.Args) != len(want) {
		t.Fatalf("%s has %d arguments, want %d", message.Name, len(message.Args), len(want))
	}
	for i := range want {
		if got := message.Args[i].Type; got != want[i] {
			t.Errorf("%s.%s type = %q, want %q", message.Name, message.Args[i].Name, got, want[i])
		}
	}
}

// Version 1 is the whole interface: nothing more may be exposed to the
// controller, because every operation here crosses the trust boundary.
func TestProtocolVersion1Surface(t *testing.T) {
	protocol := loadProtocol(t)

	if protocol.Name != "wlvision_control" {
		t.Errorf("protocol name = %q, want %q", protocol.Name, "wlvision_control")
	}

	manager := findInterface(t, protocol, "wlvision_control_v1")
	if manager.Version != 1 {
		t.Errorf("manager version = %d, want 1", manager.Version)
	}
	if got := messageNames(manager.Requests); !equalStrings(got, managerRequests) {
		t.Errorf("manager requests = %v, want %v", got, managerRequests)
	}
	if len(manager.Events) != 0 {
		t.Errorf("manager declares events %v, want none", messageNames(manager.Events))
	}

	controller := findInterface(t, protocol, "wlvision_controller_v1")
	if controller.Version != 1 {
		t.Errorf("controller version = %d, want 1", controller.Version)
	}
	if got := messageNames(controller.Requests); !equalStrings(got, controllerRequests) {
		t.Errorf("controller requests = %v, want %v", got, controllerRequests)
	}
	if got := messageNames(controller.Events); !equalStrings(got, controllerEvents) {
		t.Errorf("controller events = %v, want %v", got, controllerEvents)
	}
}

// The manager hands out exactly one kind of object, and that object is the
// controller. A second controller is rejected by the module, not by a second
// interface.
func TestManagerCreatesController(t *testing.T) {
	protocol := loadProtocol(t)
	manager := findInterface(t, protocol, "wlvision_control_v1")

	create := findMessage(t, manager.Requests, "create_controller")
	if len(create.Args) != 1 {
		t.Fatalf("create_controller arguments = %v, want exactly the new controller", argNames(create))
	}
	if create.Args[0].Type != "new_id" {
		t.Errorf("create_controller argument type = %q, want new_id", create.Args[0].Type)
	}
	if create.Args[0].Interface != "wlvision_controller_v1" {
		t.Errorf("create_controller creates %q, want wlvision_controller_v1", create.Args[0].Interface)
	}

	destroy := findMessage(t, manager.Requests, "destroy")
	if destroy.Type != "destructor" {
		t.Error("manager destroy is not marked as a destructor")
	}
}

// Every request is correlated by a caller-supplied request ID so the module can
// answer out of order, and every window operation carries the revision the
// caller saw and an opaque handle.
func TestRequestsAreCorrelatedAndRevisionSafe(t *testing.T) {
	protocol := loadProtocol(t)
	controller := findInterface(t, protocol, "wlvision_controller_v1")

	windowRequests := map[string][]string{
		"activate":       {"request_id", "handle", "revision_hi", "revision_lo"},
		"move":           {"request_id", "handle", "revision_hi", "revision_lo", "x", "y"},
		"resize":         {"request_id", "handle", "revision_hi", "revision_lo", "width", "height"},
		"close":          {"request_id", "handle", "revision_hi", "revision_lo"},
		"pointer_motion": {"request_id", "x", "y"},
		"pointer_button": {"request_id", "button", "state"},
		"pointer_axis":   {"request_id", "axis", "value"},
		"key":            {"request_id", "key", "state"},
	}

	for name, want := range windowRequests {
		requireArgs(t, findMessage(t, controller.Requests, name), want)
	}

	snapshot := findMessage(t, controller.Requests, "snapshot")
	requireArgs(t, snapshot, []string{"request_id"})

	authorize := findMessage(t, controller.Requests, "authorize_capture")
	requireArgs(t, authorize, []string{"request_id"})
}

// Handles are opaque strings owned by the session. No Wayland object, pointer
// or toolkit concept may appear in the protocol.
func TestHandlesAreOpaqueStrings(t *testing.T) {
	protocol := loadProtocol(t)

	for _, iface := range protocol.Interfaces {
		for _, message := range append(append([]messageXML{}, iface.Requests...), iface.Events...) {
			for _, arg := range message.Args {
				if arg.Name == "handle" && arg.Type != "string" {
					t.Errorf("%s.%s: handle type = %q, want string", iface.Name, message.Name, arg.Type)
				}
				if arg.Type == "object" || arg.Type == "new_id" {
					if arg.Interface != "" && arg.Interface != "wlvision_controller_v1" {
						t.Errorf("%s.%s: %s references foreign interface %q", iface.Name, message.Name, arg.Name, arg.Interface)
					}
				}
			}
		}
	}

	forbidden := []string{"wl_surface", "wl_seat", "wl_output", "x11", "xdg_", "touch", "maximize", "restore"}
	joined := strings.ToLower(strings.Join(append(messageNames(findInterface(t, protocol, "wlvision_controller_v1").Requests),
		messageNames(findInterface(t, protocol, "wlvision_controller_v1").Events)...), " "))
	for _, needle := range forbidden {
		if strings.Contains(joined, needle) {
			t.Errorf("protocol mentions %q; the control protocol stays toolkit and X11 free", needle)
		}
	}
}

// Toplevel events carry everything the CLI reports about a window, and frame
// events carry what temporal vision needs to correlate captures.
func TestEventPayloadsAreObservable(t *testing.T) {
	protocol := loadProtocol(t)
	controller := findInterface(t, protocol, "wlvision_controller_v1")

	changed := findMessage(t, controller.Events, "toplevel_changed")
	requireArgs(t, changed, []string{
		"handle", "title", "app_id", "x", "y", "width", "height",
		"state", "revision_hi", "revision_lo",
	})

	snapshot := findMessage(t, controller.Events, "snapshot")
	requireArgs(t, snapshot, []string{"revision_hi", "revision_lo"})

	removed := findMessage(t, controller.Events, "toplevel_removed")
	requireArgs(t, removed, []string{"handle"})

	done := findMessage(t, controller.Events, "request_done")
	requireArgs(t, done, []string{"request_id", "revision_hi", "revision_lo"})

	failed := findMessage(t, controller.Events, "request_failed")
	requireArgs(t, failed, []string{"request_id", "code", "message"})

	frame := findMessage(t, controller.Events, "frame")
	requireArgs(t, frame, []string{"capture_request_id", "frame_sequence"})

	authorized := findMessage(t, controller.Events, "capture_authorized")
	requireArgs(t, authorized, []string{"capture_request_id"})

	configured := findMessage(t, controller.Events, "resize_configured")
	requireArgs(t, configured, []string{"request_id", "width", "height"})
	requireArgTypes(t, configured, []string{"uint", "int", "int"})

	resizeDone := findMessage(t, controller.Events, "resize_done")
	requireArgs(t, resizeDone, []string{
		"request_id", "configured_width", "configured_height",
		"committed_width", "committed_height",
		"visible_width", "visible_height", "revision_hi", "revision_lo",
	})
	requireArgTypes(t, resizeDone, []string{
		"uint", "int", "int", "int", "int", "int", "int", "uint", "uint",
	})
}

// Failures are reported as stable codes, so the CLI can map them to its own
// error model without parsing English text.
func TestFailureCodesAreDeclared(t *testing.T) {
	protocol := loadProtocol(t)
	controller := findInterface(t, protocol, "wlvision_controller_v1")

	var errorEnum *enumXML
	for i := range controller.Enums {
		if controller.Enums[i].Name == "error" {
			errorEnum = &controller.Enums[i]
		}
	}
	if errorEnum == nil {
		t.Fatal("controller declares no error enum")
	}

	want := []string{
		"stale_revision",
		"window_not_found",
		"not_authorized",
		"capture_unavailable",
		"invalid_argument",
	}
	got := make([]string, 0, len(errorEnum.Entries))
	for _, entry := range errorEnum.Entries {
		got = append(got, entry.Name)
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !equalStrings(got, wantSorted) {
		t.Errorf("error entries = %v, want %v", got, wantSorted)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
