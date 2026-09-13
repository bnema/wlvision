package input

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
)

// keymapJSON is the embedded keyboard artifact. It is generated, never written
// by hand: keymap/keymap-dump.c compiles the RMLVO the supervisor pins for a
// session's keyboard and scripts/generate-keymap.sh stores the dump in the
// repository. Resolving text and key names through it keeps the keycodes the
// service sends in step with the layout the session actually runs.
//
//go:embed keymap/us.json
var keymapJSON []byte

// keymapSchema is the artifact schema this package understands. A document
// with a different schema is refused rather than guessed at.
const keymapSchema = "wlvision-keymap/v1"

// The RMLVO a session's supervisor pins for its keyboard, and therefore the
// only RMLVO the embedded artifact may describe. A regenerated artifact for a
// different variant, layout or option set would still parse, but its keycodes
// would belong to a keyboard the session does not have.
const (
	pinnedRules   = "evdev"
	pinnedModel   = "pc105"
	pinnedLayout  = "us"
	pinnedVariant = ""
	pinnedOptions = ""
)

// keycodeMin and keycodeMax bound the evdev keycodes the artifact may carry.
// The wire sends Params.Key with omitempty, so a keycode of 0 would travel as
// a request with no key at all rather than as an error the controller reports;
// above 255 are the synthetic keys the generator already excludes.
const (
	keycodeMin uint32 = 1
	keycodeMax uint32 = 255
)

// keymapArtifact is the decoded artifact: the RMLVO it was built from, the
// runes the layout can type, and the keys addressed by keysym name.
type keymapArtifact struct {
	Schema  string                  `json:"schema"`
	Rules   string                  `json:"rules"`
	Model   string                  `json:"model"`
	Layout  string                  `json:"layout"`
	Variant string                  `json:"variant"`
	Options string                  `json:"options"`
	Runes   map[string]keymapStroke `json:"runes"`
	Named   map[string]keymapStroke `json:"named"`
}

// keymapStroke is one keycode the artifact resolves to.
type keymapStroke struct {
	Keycode uint32 `json:"keycode"`
	Level   uint32 `json:"level"`
	Shift   bool   `json:"shift"`
}

var (
	keymapOnce     sync.Once
	keymapDecoded  *keymapArtifact
	keymapDecoding error
)

// keymap decodes the embedded artifact once. A missing or malformed artifact
// is an error at first use, never an empty keymap: a service that silently
// types nothing is worse than one that refuses to work.
func keymap() (*keymapArtifact, error) {
	keymapOnce.Do(func() {
		keymapDecoded, keymapDecoding = decodeArtifact(keymapJSON)
	})
	if keymapDecoding != nil {
		return nil, keymapDecoding
	}
	return keymapDecoded, nil
}

// decodeArtifact parses and validates one artifact document. The embedded
// document goes through this door exactly once, and a test feeds it malformed
// ones, so every refusal below is exercised rather than hoped for.
func decodeArtifact(data []byte) (*keymapArtifact, error) {
	var artifact keymapArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, fmt.Errorf("input: decoding the embedded keyboard artifact: %w", err)
	}
	if artifact.Schema != keymapSchema {
		return nil, fmt.Errorf("input: embedded keyboard artifact has schema %q, want %q",
			artifact.Schema, keymapSchema)
	}
	if artifact.Rules != pinnedRules || artifact.Model != pinnedModel || artifact.Layout != pinnedLayout ||
		artifact.Variant != pinnedVariant || artifact.Options != pinnedOptions {
		return nil, fmt.Errorf(
			"input: embedded keyboard artifact RMLVO rules=%q model=%q layout=%q variant=%q options=%q, want rules=%q model=%q layout=%q variant=%q options=%q",
			artifact.Rules, artifact.Model, artifact.Layout, artifact.Variant, artifact.Options,
			pinnedRules, pinnedModel, pinnedLayout, pinnedVariant, pinnedOptions)
	}
	if len(artifact.Runes) == 0 {
		return nil, fmt.Errorf("input: embedded keyboard artifact for layout %q has no runes", artifact.Layout)
	}
	if len(artifact.Named) == 0 {
		return nil, fmt.Errorf("input: embedded keyboard artifact for layout %q has no named keys", artifact.Layout)
	}
	for _, table := range []struct {
		name    string
		entries map[string]keymapStroke
	}{
		{"runes", artifact.Runes},
		{"named", artifact.Named},
	} {
		keys := make([]string, 0, len(table.entries))
		for key := range table.entries {
			keys = append(keys, key)
		}
		// Sorted so a bad artifact is refused with the same entry named every
		// time, whatever order the map happened to walk.
		sort.Strings(keys)
		for _, key := range keys {
			if err := validateStroke(table.entries[key]); err != nil {
				return nil, fmt.Errorf("input: embedded keyboard artifact %s[%q]: %w", table.name, key, err)
			}
		}
	}
	return &artifact, nil
}

// validateStroke checks one artifact entry against the shape the service can
// send.
func validateStroke(entry keymapStroke) error {
	if entry.Keycode < keycodeMin || entry.Keycode > keycodeMax {
		return fmt.Errorf("keycode %d is outside the evdev range %d..%d", entry.Keycode, keycodeMin, keycodeMax)
	}
	if entry.Level > 1 {
		return fmt.Errorf("level %d is neither 0 nor 1", entry.Level)
	}
	if entry.Shift != (entry.Level == 1) {
		return fmt.Errorf("shift %t does not match level %d", entry.Shift, entry.Level)
	}
	return nil
}

// Layout returns the layout the embedded artifact was generated for, such as
// "us".
//
// The artifact is compiled into the binary, so a malformed one is a build
// defect. Layout panics in that case rather than returning a layout that does
// not describe the keycodes the service sends.
func Layout() string {
	artifact, err := keymap()
	if err != nil {
		panic(err)
	}
	return artifact.Layout
}

// Stroke is one key transition: the evdev keycode to send, whether shift must
// be held for it, and, for a stroke that came from text, the character it
// types. Name is set instead of Rune when the stroke was resolved from a
// keysym name.
type Stroke struct {
	Keycode uint32
	Shift   bool
	Rune    rune
	Name    string
}

// Sequence resolves the whole text into the strokes that type it.
//
// The text is validated before any stroke is returned, so a caller that sends
// what it gets back either types all of the text or none of it. A character
// the layout cannot type is a usage error naming the character and its byte
// offset.
func Sequence(text string) ([]Stroke, error) {
	artifact, err := keymap()
	if err != nil {
		return nil, err
	}

	strokes := make([]Stroke, 0, len(text))
	for offset, character := range text {
		entry, ok := artifact.Runes[string(character)]
		if !ok {
			failure := result.NewFailure(result.CodeUsageError, agentapi.OpKey,
				"the %s keymap cannot type the character at byte %d", artifact.Layout, offset)
			failure.Details = map[string]string{
				"character": strconv.QuoteRune(character),
				"offset":    strconv.Itoa(offset),
			}
			return nil, failure
		}
		strokes = append(strokes, Stroke{
			Keycode: entry.Keycode,
			Shift:   entry.Shift,
			Rune:    character,
		})
	}
	return strokes, nil
}

// Named resolves a keysym name such as "Return" or "Shift_L" through the
// embedded artifact.
//
// Names are case sensitive: "Return" resolves and "return" does not. An
// unknown name is a usage error naming it.
func Named(name string) (Stroke, error) {
	artifact, err := keymap()
	if err != nil {
		return Stroke{}, err
	}
	entry, ok := artifact.Named[name]
	if !ok {
		failure := result.NewFailure(result.CodeUsageError, agentapi.OpKey,
			"the %s keymap has no key named %q", artifact.Layout, name)
		failure.Details = map[string]string{"name": name}
		return Stroke{}, failure
	}
	return Stroke{Keycode: entry.Keycode, Shift: entry.Shift, Name: name}, nil
}
