# Vendored protocol XML

## weston-output-capture.xml

Byte-identical copy of `protocol/weston-output-capture.xml` from Weston
`16.0.0` (commit `d1882b0a544ae2197b597a6e39478e719bc54302`), taken from the
pinned source archive recorded in `../images/arch/weston.lock`. Do not edit it:
`scripts/check-weston-baseline.sh` fails when its digest no longer matches the
pin, which is what makes drift visible.

Weston generates this protocol internally and does not install it, so wlvision
keeps its own copy and generates the capture bindings from it. The project-owned
`wlvision-control.xml` will live next to it.
