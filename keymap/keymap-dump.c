/*
 * keymap-dump.c dumps the keyboard layout wlvision pins for a session.
 *
 * It compiles an xkbcommon keymap from exactly the RMLVO values the supervisor
 * pins for the session's keyboard -- rules=evdev, model=pc105, layout=us,
 * variant="" and options="" -- and writes the subset of it the host-side input
 * service needs: the runes the layout can type, with their evdev keycode, level
 * and shift flag, and the named keys that are reachable without a modifier.
 * The two tables are made to agree for the control runes, so Sequence("\n") and
 * Named("Return") send the same keycode.
 *
 * Every "keycode" it writes is an EVDEV keycode, never an XKB one. XKB numbers
 * keys eight higher -- XKB 38 is evdev 30, the 'a' key -- and Weston adds that
 * same offset once more when it forwards an injected keycode to a client, so
 * an artifact of raw XKB keycodes would type the wrong characters.
 *
 * The artifact is generated rather than hand-written so a keycode the service
 * sends cannot silently drift from the layout the session's compositor uses.
 *
 * The output is deterministic: runes are sorted by codepoint, named keys by
 * name, every field is written in a fixed order, and nothing depends on the
 * clock, the environment or map iteration order. The document ends with one
 * newline. See keymap/README.md, and build it with
 * scripts/generate-keymap.sh.
 */

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <xkbcommon/xkbcommon-keysyms.h>
#include <xkbcommon/xkbcommon.h>

/* The RMLVO values the supervisor pins for the session's keyboard. */
static const char *const RULES = "evdev";
static const char *const MODEL = "pc105";
static const char *const LAYOUT = "us";
static const char *const VARIANT = "";
static const char *const OPTIONS = "";

/* XKB keycodes are evdev keycodes offset by 8: XKB 38 is evdev 30, the 'a'
 * key, and XKB 9 is evdev 1, Escape. The artifact carries the evdev number. */
#define XKB_EVDEV_OFFSET 8

/* The evdev keycode range the artifact covers. The upper bound excludes the
 * synthetic <I4xx> keys xkeyboard-config's inet component defines -- KEY_EURO
 * is evdev 435, for example -- which no pc105 key produces and which the
 * session's seat cannot receive. */
#define MIN_EVDEV_KEYCODE 1
#define MAX_EVDEV_KEYCODE 255

/* Keycodes the service addresses by keysym name. Only the ones a layout
 * actually produces are emitted. */
static const char *const NAMED_KEYS[] = {
    "Return", "Escape",  "Tab",     "BackSpace", "Delete", "Insert",
    "Home",   "End",     "Page_Up", "Page_Down", "Up",     "Down",
    "Left",   "Right",   "space",   "F1",        "F2",     "F3",
    "F4",     "F5",      "F6",      "F7",        "F8",     "F9",
    "F10",    "F11",     "F12",     "Shift_L",   "Control_L", "Alt_L",
    "Super_L", "Caps_Lock", "Menu", "Print",     "KP_Enter",
};

/* Control runes that xkbcommon gives a keysym of their own -- '\n' becomes
 * XK_Linefeed on its own key -- but that clients expect to arrive as a named
 * key. The generator rewrites the rune table to the keycode the named table
 * resolves, so the two paths through the service cannot disagree. */
struct control_rune {
    uint32_t rune;
    const char *name;
};
static const struct control_rune CONTROL_RUNES[] = {
    {0x08, "BackSpace"},
    {0x09, "Tab"},
    {0x0a, "Return"},
    {0x0d, "Return"},
    {0x1b, "Escape"},
    {0x7f, "Delete"},
};

/* Capacity bounds. They are far above what a pc105 layout needs; hitting one
 * is a build defect and is reported instead of silently truncating. */
#define MAX_RUNES 4096
#define MAX_SYMS 4096
/* The most modifier masks one level of one key can report. */
#define MAX_MASKS 8

struct rune_entry {
    uint32_t rune;
    int keycode;
    int level;
    bool shift;
};

struct sym_entry {
    xkb_keysym_t sym;
    int keycode;
    int level;
};

struct named_entry {
    const char *name;
    int keycode;
    int level;
    bool found;
};

/* compare_runes orders by codepoint, then by keycode and level, so the first
 * entry of each rune is the lowest keycode at its lowest level. */
static int compare_runes(const void *left, const void *right) {
    const struct rune_entry *a = left;
    const struct rune_entry *b = right;
    if (a->rune != b->rune) {
        return a->rune < b->rune ? -1 : 1;
    }
    if (a->keycode != b->keycode) {
        return a->keycode < b->keycode ? -1 : 1;
    }
    return a->level < b->level ? -1 : a->level > b->level;
}

/* compare_named orders named keys by name. */
static int compare_named(const void *left, const void *right) {
    const struct named_entry *a = left;
    const struct named_entry *b = right;
    return strcmp(a->name, b->name);
}

/* utf8_encode writes rune as UTF-8 into out, which must hold five bytes, and
 * returns the number of bytes written. */
static int utf8_encode(uint32_t rune, char out[5]) {
    if (rune < 0x80) {
        out[0] = (char)rune;
        out[1] = '\0';
        return 1;
    }
    if (rune < 0x800) {
        out[0] = (char)(0xC0 | (rune >> 6));
        out[1] = (char)(0x80 | (rune & 0x3F));
        out[2] = '\0';
        return 2;
    }
    if (rune < 0x10000) {
        out[0] = (char)(0xE0 | (rune >> 12));
        out[1] = (char)(0x80 | ((rune >> 6) & 0x3F));
        out[2] = (char)(0x80 | (rune & 0x3F));
        out[3] = '\0';
        return 3;
    }
    out[0] = (char)(0xF0 | (rune >> 18));
    out[1] = (char)(0x80 | ((rune >> 12) & 0x3F));
    out[2] = (char)(0x80 | ((rune >> 6) & 0x3F));
    out[3] = (char)(0x80 | (rune & 0x3F));
    out[4] = '\0';
    return 4;
}

/* print_json_string writes text as a JSON string, escaping what JSON requires. */
static void print_json_string(const char *text) {
    putchar('"');
    for (const unsigned char *p = (const unsigned char *)text; *p != '\0'; p++) {
        switch (*p) {
        case '"':
            fputs("\\\"", stdout);
            break;
        case '\\':
            fputs("\\\\", stdout);
            break;
        case '\b':
            fputs("\\b", stdout);
            break;
        case '\f':
            fputs("\\f", stdout);
            break;
        case '\n':
            fputs("\\n", stdout);
            break;
        case '\r':
            fputs("\\r", stdout);
            break;
        case '\t':
            fputs("\\t", stdout);
            break;
        default:
            if (*p < 0x20) {
                printf("\\u%04x", *p);
            } else {
                putchar(*p);
            }
        }
    }
    putchar('"');
}

int main(void) {
    struct xkb_context *context = xkb_context_new(XKB_CONTEXT_NO_FLAGS);
    if (context == NULL) {
        fprintf(stderr, "keymap-dump: cannot create an xkb context\n");
        return EXIT_FAILURE;
    }

    const struct xkb_rule_names names = {
        .rules = RULES,
        .model = MODEL,
        .layout = LAYOUT,
        .variant = VARIANT,
        .options = OPTIONS,
    };
    struct xkb_keymap *keymap =
        xkb_keymap_new_from_names(context, &names, XKB_KEYMAP_COMPILE_NO_FLAGS);
    if (keymap == NULL) {
        fprintf(stderr, "keymap-dump: cannot compile %s/%s/%s/%s/%s\n", RULES,
                MODEL, LAYOUT, VARIANT, OPTIONS);
        xkb_context_unref(context);
        return EXIT_FAILURE;
    }

    struct xkb_state *state = xkb_state_new(keymap);
    if (state == NULL) {
        fprintf(stderr, "keymap-dump: cannot create a keymap state\n");
        xkb_keymap_unref(keymap);
        xkb_context_unref(context);
        return EXIT_FAILURE;
    }

    const xkb_mod_index_t shift_index =
        xkb_keymap_mod_get_index(keymap, XKB_MOD_NAME_SHIFT);
    const xkb_mod_mask_t shift_mask =
        shift_index == XKB_MOD_INVALID ? 0 : (xkb_mod_mask_t)1u << shift_index;

    static struct rune_entry runes[MAX_RUNES];
    static struct sym_entry syms[MAX_SYMS];
    size_t rune_count = 0;
    size_t sym_count = 0;

    for (xkb_keycode_t keycode = MIN_EVDEV_KEYCODE + XKB_EVDEV_OFFSET;
         keycode <= MAX_EVDEV_KEYCODE + XKB_EVDEV_OFFSET; keycode++) {
        const xkb_layout_index_t levels =
            xkb_keymap_num_levels_for_key(keymap, keycode, 0);
        for (xkb_layout_index_t level = 0; level < levels; level++) {
            xkb_mod_mask_t masks[MAX_MASKS];
            const size_t mask_count = xkb_keymap_key_get_mods_for_level(
                keymap, keycode, 0, level, masks, MAX_MASKS);
            for (size_t i = 0; i < mask_count; i++) {
                const xkb_mod_mask_t mask = masks[i];

                /* Only an unmodified level and the shift level are part of the
                 * artifact: any other mask would make a stroke depend on a
                 * modifier the service does not send. */
                bool shift;
                if (mask == 0) {
                    shift = false;
                } else if (shift_mask != 0 && mask == shift_mask) {
                    shift = true;
                } else {
                    continue;
                }

                xkb_state_update_mask(state, mask, 0, 0, 0, 0, 0);
                const xkb_keysym_t sym = xkb_state_key_get_one_sym(state, keycode);
                uint32_t rune = xkb_state_key_get_utf32(state, keycode);
                if (rune == 0) {
                    rune = xkb_keysym_to_utf32(sym);
                }

                /* The artifact carries evdev keycodes, never XKB ones. */
                const int evdev = (int)keycode - XKB_EVDEV_OFFSET;
                if (evdev < MIN_EVDEV_KEYCODE || evdev > MAX_EVDEV_KEYCODE) {
                    continue;
                }

                if (rune != 0) {
                    if (rune_count >= MAX_RUNES) {
                        fprintf(stderr, "keymap-dump: more than %d runes\n", MAX_RUNES);
                        xkb_state_unref(state);
                        xkb_keymap_unref(keymap);
                        xkb_context_unref(context);
                        return EXIT_FAILURE;
                    }
                    runes[rune_count].rune = rune;
                    runes[rune_count].keycode = evdev;
                    runes[rune_count].level = shift ? 1 : 0;
                    runes[rune_count].shift = shift;
                    rune_count++;
                }

                /* Named keys are addressed without a modifier, so only the
                 * unmodified level contributes a keysym. */
                if (mask == 0 && sym != XKB_KEY_NoSymbol) {
                    if (sym_count >= MAX_SYMS) {
                        fprintf(stderr, "keymap-dump: more than %d keysyms\n", MAX_SYMS);
                        xkb_state_unref(state);
                        xkb_keymap_unref(keymap);
                        xkb_context_unref(context);
                        return EXIT_FAILURE;
                    }
                    syms[sym_count].sym = sym;
                    syms[sym_count].keycode = evdev;
                    syms[sym_count].level = 0;
                    sym_count++;
                }
            }
        }
    }

    const size_t named_total = sizeof(NAMED_KEYS) / sizeof(NAMED_KEYS[0]);
    struct named_entry named[sizeof(NAMED_KEYS) / sizeof(NAMED_KEYS[0])];
    for (size_t i = 0; i < named_total; i++) {
        named[i].name = NAMED_KEYS[i];
        named[i].keycode = 0;
        named[i].level = 0;
        named[i].found = false;

        const xkb_keysym_t wanted =
            xkb_keysym_from_name(NAMED_KEYS[i], XKB_KEYSYM_NO_FLAGS);
        if (wanted == XKB_KEY_NoSymbol) {
            continue;
        }
        /* syms are collected in ascending keycode order, so the first match is
         * the lowest keycode that produces the keysym. */
        for (size_t s = 0; s < sym_count; s++) {
            if (syms[s].sym == wanted) {
                named[i].keycode = syms[s].keycode;
                named[i].level = syms[s].level;
                named[i].found = true;
                break;
            }
        }
    }

    /* Make the rune table agree with the named table for the control runes.
     * xkbcommon maps '\n' to Linefeed on its own key, but a client treats
     * Return as Enter, and the host must send one keycode for one logical key.
     * Named keys are addressed without a modifier, so a patch is level 0 with
     * no shift. */
    if (rune_count + sizeof(CONTROL_RUNES) / sizeof(CONTROL_RUNES[0]) > MAX_RUNES) {
        fprintf(stderr, "keymap-dump: more than %d runes\n", MAX_RUNES);
        xkb_state_unref(state);
        xkb_keymap_unref(keymap);
        xkb_context_unref(context);
        return EXIT_FAILURE;
    }
    for (size_t i = 0; i < sizeof(CONTROL_RUNES) / sizeof(CONTROL_RUNES[0]); i++) {
        int keycode = -1;
        for (size_t n = 0; n < named_total; n++) {
            if (strcmp(named[n].name, CONTROL_RUNES[i].name) == 0) {
                keycode = named[n].keycode;
                break;
            }
        }
        if (keycode < 0) {
            /* The layout has no such key; there is nothing to agree with. */
            continue;
        }
        bool patched = false;
        for (size_t r = 0; r < rune_count; r++) {
            if (runes[r].rune != CONTROL_RUNES[i].rune) {
                continue;
            }
            runes[r].keycode = keycode;
            runes[r].level = 0;
            runes[r].shift = false;
            patched = true;
        }
        if (!patched) {
            runes[rune_count].rune = CONTROL_RUNES[i].rune;
            runes[rune_count].keycode = keycode;
            runes[rune_count].level = 0;
            runes[rune_count].shift = false;
            rune_count++;
        }
    }

    qsort(runes, rune_count, sizeof(runes[0]), compare_runes);
    qsort(named, named_total, sizeof(named[0]), compare_named);

    fputs("{\"schema\":\"wlvision-keymap/v1\",\"rules\":", stdout);
    print_json_string(RULES);
    fputs(",\"model\":", stdout);
    print_json_string(MODEL);
    fputs(",\"layout\":", stdout);
    print_json_string(LAYOUT);
    fputs(",\"variant\":", stdout);
    print_json_string(VARIANT);
    fputs(",\"options\":", stdout);
    print_json_string(OPTIONS);

    fputs(",\"runes\":{", stdout);
    bool first = true;
    for (size_t i = 0; i < rune_count; i++) {
        /* Duplicates are adjacent: the sort puts equal runes together, and the
         * first one is the lowest keycode at the lowest level. */
        if (i > 0 && runes[i].rune == runes[i - 1].rune) {
            continue;
        }
        char key[5];
        utf8_encode(runes[i].rune, key);
        if (!first) {
            putchar(',');
        }
        first = false;
        print_json_string(key);
        printf(":{\"keycode\":%d,\"level\":%d,\"shift\":%s}", runes[i].keycode,
               runes[i].level, runes[i].shift ? "true" : "false");
    }

    fputs("},\"named\":{", stdout);
    first = true;
    for (size_t i = 0; i < named_total; i++) {
        if (!named[i].found) {
            continue;
        }
        if (!first) {
            putchar(',');
        }
        first = false;
        print_json_string(named[i].name);
        printf(":{\"keycode\":%d,\"level\":%d,\"shift\":false}", named[i].keycode,
               named[i].level);
    }

    fputs("}}\n", stdout);

    xkb_state_unref(state);
    xkb_keymap_unref(keymap);
    xkb_context_unref(context);
    return EXIT_SUCCESS;
}
