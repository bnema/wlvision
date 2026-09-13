# GTK fixture

`wlvision.toml` is the manifest the Phase 6 gate
`TestManifestBuildThenOfflineRun` (in `test/integration/manifest_test.go`)
builds and runs. It exists so the build-to-runtime boundary is proven with a
real GTK application rather than a shell script that pretends to be one.

## Why this fixture exists

The gate proves three things at once:

1. `internal/manifest` turns a manifest into an image against the real Docker
   adapter, with the build's one allowed network use (a package install) and
   nothing else.
2. The built image, run through the CLI as a session, executes a real GTK
   application that draws a window, takes keyboard input through the
   compositor, and answers on stdout.
3. The build's network, credentials, and proxy configuration are absent from
   the session at runtime: the container is created with `--network=none` and a
   read-only root, no host path is bind-mounted, and the application's
   environment carries none of the build-side proxy or engine credentials.

## Package chosen: `zenity`

`zenity` (4.2.2-1 in Arch `extra`) depends on `gtk4` and `libadwaita`, so it is
a real GTK application. It is the smallest packaged GTK program that

- opens exactly one window with a settable title, so `wlvision windows` can
  observe it before any input is injected;
- focuses a text entry, so `wlvision type` exercises the whole keyboard path;
- prints exactly what the user typed on stdout when the entry is accepted, and
  exits 0 only then, so the echo is proof the GTK widget received the injected
  keys and not merely that the compositor accepted them.

`zenity` is installed by the manifest's `base.packages`, which `manifest.Plan`
renders as one `pacman -Sy --noconfirm zenity` transaction.

## The command the gate runs

```
wlvision run --session <id> --env GDK_BACKEND=wayland -- \
    /usr/bin/zenity --entry --title=wlvision-gtk-fixture --text="Type a word and press Return"
```

then, through the CLI:

```
wlvision activate --session <id> --window <handle>
wlvision type     --session <id> wlvision-gtk-42
wlvision key      --session <id> --name Return
```

The gate requires a window titled `wlvision-gtk-fixture` from `windows` before
it injects anything, and it requires the application to print a line equal to
exactly `wlvision-gtk-42` and to exit 0.

## The base image id

`wlvision-session:local` is built locally and has no registry digest, so
`base.digest` must name the exact image id the engine reports:

```
docker image inspect --format '{{.Id}}' wlvision-session:local
```

The gate prints that id and substitutes it for the
`sha256:REPLACE_WITH_SESSION_IMAGE_ID` placeholder before the manifest is
loaded, so rebuilding the session image needs no edit here. (The placeholder is
not a valid image id on purpose: a manifest that skipped the substitution fails
validation instead of building something.) To build the fixture by hand,
substitute the printed id into a copy of this file first.
