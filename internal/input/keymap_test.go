package input

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/result"
)

// TestEmbeddedKeymapIsThePinnedUSLayout checks the artifact itself: the schema
// and the RMLVO values the generator and the session's supervisor must agree
// on.
func TestEmbeddedKeymapIsThePinnedUSLayout(t *testing.T) {
	raw, err := os.ReadFile("keymap/us.json")
	if err != nil {
		t.Fatalf("read the artifact: %v", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Error("the artifact does not end with exactly one newline")
	}
	var document struct {
		Schema  string `json:"schema"`
		Rules   string `json:"rules"`
		Model   string `json:"model"`
		Layout  string `json:"layout"`
		Variant string `json:"variant"`
		Options string `json:"options"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode the artifact: %v", err)
	}
	if document.Schema != keymapSchema {
		t.Errorf("schema = %q, want %q", document.Schema, keymapSchema)
	}
	if document.Rules != "evdev" || document.Model != "pc105" || document.Layout != "us" ||
		document.Variant != "" || document.Options != "" {
		t.Errorf("RMLVO = %s/%s/%s/%s/%s, want evdev/pc105/us/\"\"/\"\"",
			document.Rules, document.Model, document.Layout, document.Variant, document.Options)
	}
	if got := Layout(); got != "us" {
		t.Errorf("Layout() = %q, want %q", got, "us")
	}
}

func TestKeymapResolvesTheUSRunes(t *testing.T) {
	artifact, err := keymap()
	if err != nil {
		t.Fatalf("keymap: %v", err)
	}

	cases := []struct {
		character rune
		keycode   uint32
		level     uint32
		shift     bool
	}{
		{'a', 30, 0, false},
		{'A', 30, 1, true},
		{'1', 2, 0, false},
		{'!', 2, 1, true},
		{' ', 57, 0, false},
		{'z', 44, 0, false},
		{'Z', 44, 1, true},
		{',', 51, 0, false},
		{'<', 51, 1, true},
	}
	for _, c := range cases {
		entry, ok := artifact.Runes[string(c.character)]
		if !ok {
			t.Errorf("runes[%q] is missing", c.character)
			continue
		}
		if entry.Keycode != c.keycode || entry.Level != c.level || entry.Shift != c.shift {
			t.Errorf("runes[%q] = %d/level %d/shift %t, want %d/level %d/shift %t",
				c.character, entry.Keycode, entry.Level, entry.Shift, c.keycode, c.level, c.shift)
		}
	}
}

func TestKeymapHasTheNamedKeys(t *testing.T) {
	artifact, err := keymap()
	if err != nil {
		t.Fatalf("keymap: %v", err)
	}

	want := []string{
		"Return", "Escape", "Tab", "BackSpace", "Delete", "Insert",
		"Home", "End", "Page_Up", "Page_Down", "Up", "Down", "Left", "Right",
		"space", "F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12",
		"Shift_L", "Control_L", "Alt_L", "Super_L", "Caps_Lock", "Menu", "Print", "KP_Enter",
	}
	for _, name := range want {
		entry, ok := artifact.Named[name]
		if !ok {
			t.Errorf("named[%q] is missing", name)
			continue
		}
		if entry.Keycode == 0 {
			t.Errorf("named[%q] has no keycode", name)
		}
		if entry.Level != 0 || entry.Shift {
			t.Errorf("named[%q] = level %d/shift %t, want an unshifted level 0", name, entry.Level, entry.Shift)
		}
	}

	cases := []struct {
		name    string
		keycode uint32
	}{
		{"Return", 28},
		{"Shift_L", 42},
		{"BackSpace", 14},
		{"space", 57},
	}
	for _, c := range cases {
		stroke, err := Named(c.name)
		if err != nil {
			t.Errorf("Named(%q): %v", c.name, err)
			continue
		}
		if stroke.Keycode != c.keycode || stroke.Name != c.name {
			t.Errorf("Named(%q) = %+v, want keycode %d", c.name, stroke, c.keycode)
		}
	}
}

func TestSequenceTypesHelloWorld(t *testing.T) {
	strokes, err := Sequence("Hello, world!")
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	want := []Stroke{
		{Keycode: 35, Shift: true, Rune: 'H'},
		{Keycode: 18, Rune: 'e'},
		{Keycode: 38, Rune: 'l'},
		{Keycode: 38, Rune: 'l'},
		{Keycode: 24, Rune: 'o'},
		{Keycode: 51, Rune: ','},
		{Keycode: 57, Rune: ' '},
		{Keycode: 17, Rune: 'w'},
		{Keycode: 24, Rune: 'o'},
		{Keycode: 19, Rune: 'r'},
		{Keycode: 38, Rune: 'l'},
		{Keycode: 32, Rune: 'd'},
		{Keycode: 2, Shift: true, Rune: '!'},
	}
	if len(strokes) != len(want) {
		t.Fatalf("strokes = %+v, want %+v", strokes, want)
	}
	for i := range want {
		if strokes[i] != want[i] {
			t.Errorf("stroke %d = %+v, want %+v", i, strokes[i], want[i])
		}
	}
}

func TestSequenceRejectsACharacterTheLayoutCannotType(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		character string
		offset    string
	}{
		{"accented", "é", strconv.QuoteRune('é'), "0"},
		{"currency", "€", strconv.QuoteRune('€'), "0"},
		{"diaeresis", "ü", strconv.QuoteRune('ü'), "0"},
		{"after a supported character", "aé", strconv.QuoteRune('é'), "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Sequence(c.text)
			failure := failureOf(t, err)
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if failure.Details["character"] != c.character {
				t.Errorf("details = %v, want character %s", failure.Details, c.character)
			}
			if failure.Details["offset"] != c.offset {
				t.Errorf("details = %v, want offset %s", failure.Details, c.offset)
			}
		})
	}

	strokes, err := Sequence("")
	if err != nil || len(strokes) != 0 {
		t.Errorf("Sequence(\"\") = %v, %v, want no strokes and no error", strokes, err)
	}
}

// TestDecodeArtifactRefusesMalformedDocuments proves the artifact is validated
// rather than trusted: each document below differs from the real artifact in
// exactly one way.
func TestDecodeArtifactRefusesMalformedDocuments(t *testing.T) {
	raw, err := os.ReadFile("keymap/us.json")
	if err != nil {
		t.Fatalf("read the artifact: %v", err)
	}

	// mutate returns the artifact with one field changed by change.
	mutate := func(t *testing.T, change func(map[string]any)) []byte {
		t.Helper()
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("decode the artifact: %v", err)
		}
		change(document)
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encode the mutated artifact: %v", err)
		}
		return encoded
	}
	entry := func(t *testing.T, document map[string]any, table, key string) map[string]any {
		t.Helper()
		entries, ok := document[table].(map[string]any)
		if !ok {
			t.Fatalf("the artifact has no %s table", table)
		}
		entry, ok := entries[key].(map[string]any)
		if !ok {
			t.Fatalf("%s[%q] is missing", table, key)
		}
		return entry
	}

	cases := []struct {
		name   string
		data   []byte
		detail string
	}{
		{"truncated json", []byte(`{"schema":`), "decoding"},
		{"wrong schema", mutate(t, func(d map[string]any) { d["schema"] = "wlvision-keymap/v2" }), "schema"},
		{"wrong rules", mutate(t, func(d map[string]any) { d["rules"] = "xfree86" }), "rules"},
		{"wrong model", mutate(t, func(d map[string]any) { d["model"] = "pc104" }), "model"},
		{"wrong layout", mutate(t, func(d map[string]any) { d["layout"] = "de" }), "layout"},
		{"wrong variant", mutate(t, func(d map[string]any) { d["variant"] = "intl" }), "variant"},
		{"wrong options", mutate(t, func(d map[string]any) { d["options"] = "caps:swapescape" }), "options"},
		{"no runes", mutate(t, func(d map[string]any) { d["runes"] = map[string]any{} }), "no runes"},
		{"no named keys", mutate(t, func(d map[string]any) { d["named"] = map[string]any{} }), "no named keys"},
		{"keycode zero", mutate(t, func(d map[string]any) {
			entry(t, d, "runes", "a")["keycode"] = float64(0)
		}), `runes["a"]`},
		{"keycode above the evdev range", mutate(t, func(d map[string]any) {
			entry(t, d, "runes", "a")["keycode"] = float64(256)
		}), `runes["a"]`},
		{"level above one", mutate(t, func(d map[string]any) {
			entry(t, d, "runes", "a")["level"] = float64(2)
		}), `runes["a"]`},
		{"shift on an unshifted level", mutate(t, func(d map[string]any) {
			entry(t, d, "runes", "a")["shift"] = true
		}), `runes["a"]`},
		{"no shift on a shifted level", mutate(t, func(d map[string]any) {
			entry(t, d, "runes", "A")["shift"] = false
		}), `runes["A"]`},
		{"named key without a keycode", mutate(t, func(d map[string]any) {
			entry(t, d, "named", "Return")["keycode"] = float64(0)
		}), `named["Return"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			artifact, err := decodeArtifact(c.data)
			if err == nil {
				t.Fatalf("decodeArtifact = %+v, want a refusal", artifact)
			}
			if !strings.Contains(err.Error(), c.detail) {
				t.Errorf("error = %v, want it to name %q", err, c.detail)
			}
		})
	}

	if _, err := decodeArtifact(raw); err != nil {
		t.Errorf("decodeArtifact(the committed artifact) = %v, want it to decode", err)
	}
}

// TestSequenceAgreesWithNamedForControlRunes pins the mapping the generator
// rewrites: the rune table and the named table must not disagree about the
// same logical key, because clients treat XK_Linefeed as a line feed while
// Return is what Enter means.
func TestSequenceAgreesWithNamedForControlRunes(t *testing.T) {
	cases := []struct {
		text    string
		name    string
		keycode uint32
	}{
		{"\n", "Return", 28},
		{"\r", "Return", 28},
		{"\t", "Tab", 15},
		{"\b", "BackSpace", 14},
		{"\x1b", "Escape", 1},
		{"\x7f", "Delete", 111},
	}
	for _, c := range cases {
		strokes, err := Sequence(c.text)
		if err != nil {
			t.Fatalf("Sequence(%q): %v", c.text, err)
		}
		if len(strokes) != 1 {
			t.Fatalf("Sequence(%q) = %+v, want one stroke", c.text, strokes)
		}
		if strokes[0].Keycode != c.keycode || strokes[0].Shift {
			t.Errorf("Sequence(%q) = %+v, want keycode %d without shift", c.text, strokes[0], c.keycode)
		}
		named, err := Named(c.name)
		if err != nil {
			t.Fatalf("Named(%q): %v", c.name, err)
		}
		if strokes[0].Keycode != named.Keycode || strokes[0].Shift != named.Shift {
			t.Errorf("Sequence(%q) sends %+v but Named(%q) sends %+v", c.text, strokes[0], c.name, named)
		}
	}
}

func TestNamedIsCaseSensitive(t *testing.T) {
	stroke, err := Named("Return")
	if err != nil {
		t.Fatalf("Named(Return): %v", err)
	}
	if stroke.Keycode != 28 || stroke.Shift || stroke.Name != "Return" {
		t.Errorf("Named(Return) = %+v, want keycode 28 without shift", stroke)
	}

	_, err = Named("return")
	failure := failureOf(t, err)
	if failure.Code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
	}
	if failure.Details["name"] != "return" {
		t.Errorf("details = %v, want the name %q", failure.Details, "return")
	}
}
