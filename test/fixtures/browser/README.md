# browser fixture

A real browser in a session: the heaviest client this project supports, and the
one most likely to expose a limit the smaller fixtures never reach (dependency
tree size, memory, its own sandbox, software rendering over a headless
compositor).

`wlvision.toml` derives an image from the locally built session image, pinned
through `base.digest`, and installs `firefox` from the distribution. The page it
drives lives in `page/`: `index.html` writes what it receives into the document
title and into a visible line, so a typed value can be confirmed both by the
window metadata the CLI reports and by a screenshot, and `user.js` keeps a first
run quiet and its profile inside the session's bounded home.

The base id changes whenever the session image is rebuilt, and a manifest's
copied files are resolved against the manifest's own directory, so build from a
copy of this directory with the id substituted:

```bash
work="$(mktemp -d)"
cp -r test/fixtures/browser/. "${work}/"
id="$(docker image inspect --format '{{.Id}}' wlvision-session:local)"
sed -i "s|sha256:REPLACE_WITH_SESSION_IMAGE_ID|${id}|" "${work}/wlvision.toml"
wlvision --json image build --manifest "${work}/wlvision.toml" --tag wlvision-browser:local
wlvision --json session create --session browser --image wlvision-browser:local --wait
tar -C "${work}/page" -cf - . | wlvision --json inject --session browser --bundle page
wlvision --json run --session browser -- /bin/sh -c '
  mkdir -p "$HOME/profile" &&
  cp /run/wlvision/payload/page/user.js "$HOME/profile/" &&
  exec /usr/bin/firefox --profile "$HOME/profile" --no-remote file:///run/wlvision/payload/page/index.html'
```

The build needs the network for the package install; the session that runs from
the result does not have one.
