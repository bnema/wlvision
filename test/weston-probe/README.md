# Pinned-Weston probe

Proves that every Weston interface wlvision's control module is designed to
use exists at the pin recorded in `images/arch/weston.lock`:

* weston **16.0.0**, commit `d1882b0a544ae2197b597a6e39478e719bc54302`
* archive `source_archive_sha256 = 6a81af51045ccb2813f3a1d63ff8a66c121743c1a5ff3ce78388660bf650c55c`
* libweston ABI major **16** (`libweston-16.so.0`, `pkg-config` version `16.0.0`)

The Weston revision is built in a container from the pinned archive and
`probe.c` is compiled against the freshly built library as a shared module.
`probe.c` is never executed; it exists so that the compiler and linker have
to resolve every symbol the control module will use.

## Commands

```sh
# 1. inputs stage - the only stage with network access
docker build -t wlvision-weston-inputs:local \
    -f test/weston-probe/Containerfile.inputs test/weston-probe

# 2. verification stage - must run with no network at all
docker build --network=none -t wlvision-weston-probe:local \
    -f test/weston-probe/Containerfile test/weston-probe

# 3. acceptance: the module exists and links libweston-16
docker run --rm wlvision-weston-probe:local sh -c 'ls -l /probe.so && ldd /probe.so | head'
```

The probe image `FROM wlvision-weston-inputs:local`, so step 1 must run first
(or at least once) on the same machine; step 2 never pulls or fetches.

## Resolved base image

`archlinux:base-devel`, resolved on 2026-09-13:

| reference | digest |
| --- | --- |
| OCI image index | `sha256:61f7de2dd88cc4ba1fe36c24cfe1a503c3936984492d6405eeab013ce6ac68c5` |
| `linux/amd64` manifest (the one pinned in `FROM`) | `sha256:5b987b0196907ea97dd10e7b48e7d403c35c0389986da015aa86cf8d0b058084` |

Image annotations: `org.opencontainers.image.created=2026-09-08T19:09:22Z`,
`org.opencontainers.image.revision=9b7cd5e184f371f7e5aecd406ff9d2f42c883b07`.

`Containerfile.inputs`:

```
FROM archlinux:base-devel@sha256:5b987b0196907ea97dd10e7b48e7d403c35c0389986da015aa86cf8d0b058084
```

## Dependencies per stage

Build context is `test/weston-probe/`, so `images/arch/weston.lock` cannot be
`COPY`ed in. The lock's `source_archive_url`, `source_archive_sha256`, the six
`file_sha256/*` digests and the version above are therefore carried in the
Containerfiles as `ARG` defaults / literals, with a comment pointing at the
lock; `scripts/check-weston-baseline.sh` keeps validating the lock itself.

### Stage 1 - `Containerfile.inputs` (network allowed)

* installed, because they fetch and verify: `curl`, `ca-certificates`
  (plus a `pacman -Syu` so the archive snapshot and the cached dependency set
  are consistent)
* downloaded but **not** installed, into `/opt/weston-deps`:
  `meson ninja wayland wayland-protocols libxkbcommon libinput libevdev
  libdrm pixman libdisplay-info cairo libpng`

`pacman -Syw --cachedir=/opt/weston-deps` puts only the exact packages the
verification stage is missing into a private cache; `pacman -U` in stage 2
installs that set without touching a mirror. (The default package cache is not
used because `pacman -Syu` leaves superseded versions there, and installing
those with `pacman -U` would downgrade the image.)

The 31 packages that end up in `/opt/weston-deps` are: cairo, default-cursors,
fontconfig, freetype2, libdisplay-info, libdrm, libevdev, libgudev, libinput,
libpciaccess, libpng, libwacom, libx11, libxau, libxcb, libxdmcp, libxext,
libxkbcommon, libxrender, lua54, lzo, meson, mtdev, ninja, pixman,
python-tqdm, wayland, wayland-protocols, xcb-proto, xkeyboard-config,
xorgproto. Several of those are pulled in by another dependency rather than
requested explicitly: `lua54` by libinput, `default-cursors` by wayland,
`xkeyboard-config` by libxkbcommon and `python-tqdm` by meson (`pacman -Qi`
"Required By").

### Stage 2 - `Containerfile` (no network)

* `pacman -U --noconfirm /opt/weston-deps/*.pkg.tar.zst`
* nothing else is installed; the Weston build and the probe compile need no
  further packages

Versions observed in the final image: meson 1.12.0, ninja 1.13.2,
gcc 16.2.1, wayland 1.26.0, wayland-protocols 1.49, libdisplay-info 0.3.0,
cairo 1.18.4, libpng 1.6.58, libdrm 2.4.134, libxkbcommon 1.13.2,
libinput 1.31.3, libevdev 1.13.7, pixman 0.46.4.

`libdisplay-info` 0.3.0 satisfies the unconditional
`>= 0.2.0, < 0.4.0` requirement, so meson never falls back to the
`subprojects/display-info.wrap` (which would need the network).

## Weston build

`meson setup build --prefix=/usr`, then `ninja -C build` and
`ninja -C build install` (the install is what makes `libweston-16.pc` and the
headers visible to `pkg-config` for the probe). Options actually used:

```
-Dbackend-default=headless -Dbackend-headless=true
-Dbackend-drm=false -Dbackend-wayland=false -Dbackend-x11=false
-Dbackend-vnc=false -Dbackend-rdp=false -Dbackend-pipewire=false
-Drenderer-gl=false -Drenderer-vulkan=false
-Dxwayland=false -Dsystemd=false -Dcolor-management-lcms=false
-Dimage-jpeg=false -Dimage-webp=false
-Dshell-desktop=false -Dshell-ivi=false -Dshell-kiosk=false -Dshell-lua=false
-Ddemo-clients=false -Dsimple-clients= -Dtests=false -Ddoc=false
```

No meson option had to be changed: these were accepted as written on the first
successful configure. `-Dbackend-default=headless` is mandatory rather than
cosmetic - meson.build errors out if the default backend is disabled, and the
default is `drm`.

The one adjustment forced by the source was in the **dependency list**, not in
the options: `meson setup` failed with

```
shared/meson.build:53:1: ERROR: Dependency "cairo" not found (tried pkg-config)
```

`shared/meson.build` builds `deps_cairo_shared` with `dependency('cairo')` and
`dependency('libpng')` unconditionally, and the headless backend links that
dependency (`libweston/backend-headless/meson.build`), so both are required
even with every shell, client, tool and test disabled. They were added to
`WESTON_BUILD_DEPS`.

## Observed results

All commands below were run from the repository root on 2026-09-13.

```
$ docker build -t wlvision-weston-inputs:local -f test/weston-probe/Containerfile.inputs test/weston-probe
# exit 0
```

The fetch/verify steps printed (from a `--no-cache` run):

```
+ echo '6a81af51045ccb2813f3a1d63ff8a66c121743c1a5ff3ce78388660bf650c55c  /tmp/weston.tar.gz'
+ sha256sum -c -
/tmp/weston.tar.gz: OK
...
+ sha256sum -c -
protocol/weston-output-capture.xml: OK
include/libweston/libweston.h: OK
include/libweston/desktop.h: OK
libweston/output-capture.h: OK
include/libweston/backend-headless.h: OK
libweston/pixman-renderer.h: OK
```

(`pacman -Syu` also prints a benign `==> ERROR: There is no secret key
available to sign with.` from the keyring hook; the build exit status is 0.)

```
$ docker build --network=none -t wlvision-weston-probe:local -f test/weston-probe/Containerfile test/weston-probe
# exit 0 -- configured, compiled, installed libweston-16 and built /probe.so with no network
#11 [5/5] RUN set -eux;     cc -shared -fPIC -Wall -Wextra        -o /probe.so /src/probe.c        $(pkg-config --cflags --libs libweston-16 wayland-server)
#11 0.080 ++ pkg-config --cflags --libs libweston-16 wayland-server
#11 0.081 + cc -shared -fPIC -Wall -Wextra -o /probe.so /src/probe.c -I/usr/include/libweston-16 -I/usr/include/pixman-1 -lweston-16 -lwayland-server -lm
```

`probe.c` compiles clean under `-Wall -Wextra`.

```
$ docker run --rm wlvision-weston-probe:local sh -c 'ls -l /probe.so && ldd /probe.so | head'
-rwxr-xr-x 1 root root 17320 Sep 13 06:49 /probe.so
	linux-vdso.so.1 (0x00007fb79f75a000)
	libweston-16.so.0 => /usr/lib/libweston-16.so.0 (0x00007fb79f6a0000)
	libwayland-server.so.0 => /usr/lib/libwayland-server.so.0 (0x00007fb79f68b000)
	libm.so.6 => /usr/lib/libm.so.6 (0x00007fb79f762000)
	libc.so.6 => /usr/lib/libc.so.6 (0x00007fb79f754000)
	libpixman-1.so.0 => /usr/lib/libpixman-1.so.0 (0x00007fb79f74d000)
	libdrm.so.2 => /usr/lib/libdrm.so.2 (0x00007fb79f744000)
	libxkbcommon.so.0 => /usr/lib/libxkbcommon.so.0 (0x00007fb79f73f000)
	libffi.so.8 => /usr/lib/libffi.so.8 (0x00007fb79f73e000)
```

`readelf -d /probe.so` gives `NEEDED libweston-16.so.0` and
`NEEDED libwayland-server.so.0`; `libweston-16.so.0` has
`SONAME libweston-16.so.0`. `wet_module_init` is exported from the module
(`nm -D --defined-only /probe.so` -> `T wet_module_init`), which is the symbol
`frontend/main.c` looks up with `dlsym()` to load a plugin.

```
$ bash scripts/check-weston-baseline.sh
weston baseline verified: 16.0.0 (d1882b0a544ae2197b597a6e39478e719bc54302), libweston ABI 16
```

`git status --short` shows only `test/weston-probe/` from this work. No shell
file was added, so there was nothing new to check with `bash -n`.

## Where each interface comes from, and whether it is exported

Provenance from the pinned source tree; export status from `nm -D
--defined-only` on the library built by `Containerfile`, run inside the probe
image (not assumed).

| Interface | Header | Header class | Exported? |
| --- | --- | --- | --- |
| `struct weston_desktop_api` with `surface_added` / `surface_removed` | `include/libweston/desktop.h` | public, installed | type only (no symbol) |
| `weston_desktop_surface_get_title` | `include/libweston/desktop.h` | public, installed | `libweston-16.so.0` |
| `weston_desktop_surface_get_app_id` | `include/libweston/desktop.h` | public, installed | `libweston-16.so.0` |
| `weston_desktop_surface_get_geometry` | `include/libweston/desktop.h` | public, installed | `libweston-16.so.0` |
| `weston_desktop_surface_set_size` | `include/libweston/desktop.h` | public, installed | `libweston-16.so.0` |
| `weston_desktop_surface_close` | `include/libweston/desktop.h` | public, installed | `libweston-16.so.0` |
| `weston_desktop_create` (installs the listeners) | `include/libweston/desktop.h` | public, installed | `libweston-16.so.0` |
| `weston_desktop_surface_get_client`, `weston_desktop_client_get_client` | `include/libweston/desktop.h` | public, installed | `libweston-16.so.0` |
| `struct weston_compositor::seat_list` | `include/libweston/libweston.h` | public, installed | struct field, no symbol |
| `weston_seat_get_pointer`, `weston_seat_get_keyboard` | `include/libweston/libweston.h` | public, installed | `libweston-16.so.0` |
| `weston_pointer_send_motion` / `_send_button` / `_send_axis` / `_send_frame` | `include/libweston/libweston.h` | public, installed | `libweston-16.so.0` |
| `weston_keyboard_send_key` / `_send_modifiers` / `_send_keymap` | `include/libweston/libweston.h` | public, installed | `libweston-16.so.0` |
| `struct weston_pointer_motion_event`, `weston_pointer_button_event`, `weston_pointer_axis_event`, `weston_key_event` | `include/libweston/libweston.h` | public, installed | types only (no symbol) |
| `wl_client_get_credentials` | `<wayland-server-core.h>` | wayland-server | `libwayland-server.so.0` |
| `wl_resource_get_client` | `<wayland-server-core.h>` | wayland-server | `libwayland-server.so.0` |
| `struct weston_output_capture_attempt`, `struct weston_output_capture_client` | `include/libweston/libweston.h` | **public, installed** | type only (no symbol) |
| `weston_compositor_add_screenshot_authority` | `include/libweston/libweston.h` | **public, installed** | `libweston-16.so.0` |
| `struct weston_compositor::output_list` | `include/libweston/libweston.h` | public, installed | struct field, no symbol |
| `struct weston_output::frame_signal` | `include/libweston/libweston.h` | public, installed | struct field, no symbol |
| `wl_signal_add` (attach the listener) | `<wayland-server-core.h>` | wayland-server | static inline: no symbol by design; it expands to `wl_list_insert`, which `libwayland-server.so.0` does export |

All 18 libweston symbols checked with `nm -D --defined-only
/usr/lib/libweston-16.so.0` came back `EXPORTED`.

**No interface the control module needs is missing, and none of them is
not-exported.** The two non-obvious negatives are worth spelling out:

* `wl_signal_add` has no dynamic symbol anywhere because it is a `static
  inline` in the public `wayland-server-core.h`; the probe links the
  `wl_list_insert` it expands to. This is expected, not a gap.
* `output->frame_signal`, `compositor->output_list` and
  `compositor->seat_list` are struct fields reached by offset into fully
  public structs, so there is no symbol to look up. That is a stronger
  position than a symbol: it means the struct layout is part of the public
  ABI of `libweston-16.so`, not a private implementation detail - but it also
  means those struct layouts must be pinned by digest, which
  `images/arch/weston.lock` already does for `include/libweston/libweston.h`.

## Findings that affect the module design

1. **The capture authority is a public interface, not a private one.** The
   lock file's comment says synchronization, input injection and the capture
   authority are private libweston interfaces. `struct
   weston_output_capture_attempt` and
   `weston_compositor_add_screenshot_authority()` are declared in the
   *installed* `include/libweston/libweston.h` (lines 2895 and 2906 of the
   source tree) and are exported from `libweston-16.so.0`. The private
   `libweston/output-capture.h` is a source-tree header that is not installed
   at all (`/usr/include/libweston-16/libweston/output-capture.h` does not
   exist after `ninja install`); it declares the renderer-side capture helpers
   and `weston_compositor_install_capture_protocol()`, and does not declare
   either of the two names above. The control module does not need any private
   header or any source tree include path - it can be built against the
   installed public headers alone.
2. **There is no separate `libweston-desktop-16.so`.** In 16.0.0 the desktop
   shell API is compiled straight into `libweston-16.so`
   (`libweston/meson.build` adds `libweston/desktop/*.c` to `srcs_libweston`),
   so one library, one pkg-config file (`libweston-16.pc`) and one SONAME
   cover both the compositor and the desktop surface API.
3. **Input injection is public too** (`libweston.h`), even though the lock
   file groups it with the private interfaces: the event structs are complete
   public types, so the module can build them directly instead of using the
   internal `*_event_init()` helpers.
4. The build needs `cairo` and `libpng` regardless of which backends, shells
   and clients are disabled (see above), plus the unconditional
   `libinput`, `libevdev`, `libdrm` and `libdisplay-info` dependencies.
   These are build-time only; nothing here is required to *link* the module.
