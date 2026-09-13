# keymap

The keyboard artifact the host-side input service resolves text and key names
with, and the program that generates it.

## Why the artifact is generated

The session's compositor receives evdev keycodes, not characters. To type the
character `A` the service must send the keycode of the `A` key along with a
shift press. Which keycode that is, and whether a character needs shift,
depends on the keyboard layout the session runs with. If the service hard-coded
those numbers they would silently drift from the layout the supervisor pins.

So the layout is compiled from its source of truth, xkbcommon, with exactly the
RMLVO values the supervisor pins for the session's keyboard:

| setting | value   |
| ------- | ------- |
| rules   | `evdev` |
| model   | `pc105` |
| layout  | `us`    |
| variant | `""`    |
| options | `""`    |

`keymap-dump.c` compiles that keymap in the local session image and dumps the
subset the service needs. `scripts/generate-keymap.sh` builds and runs it and
writes the result to `internal/input/keymap/us.json`, which
`internal/input/keymap.go` embeds with `//go:embed`. The host therefore needs
neither xkbcommon headers nor a compiler; only the local image does.

## Regenerating

```sh
scripts/generate-keymap.sh                    # rewrites internal/input/keymap/us.json
scripts/generate-keymap.sh /tmp/us.json       # writes elsewhere, for a diff
diff /tmp/us.json internal/input/keymap/us.json
```

The script requires the local image (`wlvision-weston-shell:local` by default,
overridable with `WLVISION_KEYMAP_IMAGE`). It never builds or pulls the image,
fails loudly when the image is missing or the compile fails, and cleans up its
temporary directory.

## Format

`wlvision-keymap/v1`. One JSON document, deterministic, with a trailing
newline: runes are sorted by codepoint, named keys by name, every field is
written in a fixed order, and nothing depends on the clock or the environment,
so a regenerated artifact can be diffed byte for byte.

```json
{"schema":"wlvision-keymap/v1","rules":"evdev","model":"pc105","layout":"us","variant":"","options":"","runes":{"a":{"keycode":30,"level":0,"shift":false}},"named":{"BackSpace":{"keycode":14,"level":0,"shift":false}}}
```

- `runes` maps a character to the evdev keycode that types it. An entry is
  emitted only when the level's modifier mask is exactly `{}` (`"level":0`,
  `"shift":false`) or exactly `{Shift}` (`"level":1`, `"shift":true`), so every
  entry is reachable with one shift press and nothing else. Keysyms with no
  Unicode value (modifiers, dead keys) are skipped, and when two keycodes
  produce the same character the lowest keycode at the lowest level wins.
- `keycode` is always an **evdev** keycode. XKB keycodes are evdev keycodes
  offset by 8 (XKB 38 is evdev 30, the `a` key), so the generator iterates the
  XKB keycodes for evdev 1..255 and writes `xkb_keycode - 8`. The host resolves
  text to evdev keycodes and sends those; Weston adds the offset back when it
  forwards the injected keycode to a client, which is what makes the client's
  XKB state see the key the host meant.
- The artifact is not ASCII-only. The layout maps `±` at an unshifted level
  (keypad plus-minus, evdev 118), so it is emitted and typeable. It stops at
  evdev 255 on purpose: above that range are the synthetic `<I4xx>` keys
  xkeyboard-config's inet component defines, such as `KEY_EURO` at evdev 435,
  which no pc105 key produces. Characters such as `é`, `ü` and `€` are
  therefore absent, and are what `input.Sequence` refuses.
- `named` maps a key name to the same shape for keys addressed by keysym name.
  Only names that resolve to a keycode in this layout are emitted.

### Control runes

`runes` is not limited to printable characters, and the generator rewrites the
control runes that xkbcommon gives a keysym of their own so they agree with the
named table. A client treats `Return` as Enter but `XK_Linefeed` is not Enter,
so leaving the layout's own answer in place would make `Sequence("\n")` and
`Named("Return")` disagree about the same logical key:

| rune         | named key   | evdev keycode |
| ------------ | ----------- | ------------- |
| `\b` (0x08) | `BackSpace` | 14            |
| `\t` (0x09) | `Tab`       | 15            |
| `\n` (0x0a) | `Return`    | 28            |
| `\r` (0x0d) | `Return`    | 28            |
| `\x1b`       | `Escape`    | 1             |
| `\x7f`       | `Delete`    | 111           |

### Named keys

The candidate list, in the order it is declared in `keymap-dump.c`:

`Return`, `Escape`, `Tab`, `BackSpace`, `Delete`, `Insert`, `Home`, `End`,
`Page_Up`, `Page_Down`, `Up`, `Down`, `Left`, `Right`, `space`, `F1`–`F12`,
`Shift_L`, `Control_L`, `Alt_L`, `Super_L`, `Caps_Lock`, `Menu`, `Print`,
`KP_Enter`.

Every one of those resolves in the pinned layout, so all of them are emitted.
A name that a different layout could not produce would simply be absent; that
is why the list is documented here rather than promised in the artifact.

## Keysym names are case sensitive

`input.Named` looks a name up exactly as written. `Return` resolves, `return`
does not, and an unknown name is a usage error rather than an empty stroke.
