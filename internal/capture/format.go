// Package capture turns compositor buffers into images wlvision can hand to an
// agent: canonical RGBA, a stable digest, and PNG bytes.
package capture

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
)

// Format is a wl_shm pixel format wlvision understands.
//
// Only the two formats Weston's headless pixman output actually produces are
// accepted. Anything else is refused rather than guessed at, because guessing
// silently swaps colour channels.
type Format uint32

// Accepted pixel formats, numbered as wl_shm numbers them.
const (
	FormatARGB8888 Format = 0
	FormatXRGB8888 Format = 1
)

// String names a pixel format.
func (f Format) String() string {
	switch f {
	case FormatARGB8888:
		return "argb8888"
	case FormatXRGB8888:
		return "xrgb8888"
	default:
		return fmt.Sprintf("unknown_format_%d", uint32(f))
	}
}

// DRM format codes as the compositor reports them in the capture protocol's
// format event. These are four-character codes, not the small enum wl_shm uses
// when a client creates a buffer.
const (
	drmFormatARGB8888 = 0x34325241 // 'AR24'
	drmFormatXRGB8888 = 0x34325258 // 'XR24'
)

// DRMCode returns the format's four-character code, which is what the capture
// protocol announces.
func (f Format) DRMCode() uint32 {
	switch f {
	case FormatARGB8888:
		return drmFormatARGB8888
	case FormatXRGB8888:
		return drmFormatXRGB8888
	default:
		return 0
	}
}

// FormatFromDRM maps a four-character format code to an accepted buffer
// format, refusing anything wlvision cannot decode.
func FormatFromDRM(code uint32) (Format, error) {
	switch code {
	case drmFormatARGB8888:
		return FormatARGB8888, nil
	case drmFormatXRGB8888:
		return FormatXRGB8888, nil
	default:
		return 0, fmt.Errorf("unsupported DRM format 0x%08x", code)
	}
}

// Decode converts a compositor buffer into a tightly packed RGBA image.
//
// The buffer arrives as little-endian words whose memory order is B, G, R, A
// for both formats, which is not Go's RGBA order, so the channels are
// swizzled explicitly. XRGB8888 carries no alpha and becomes fully opaque.
//
// stride is the number of bytes per row as the compositor advertised it, which
// may exceed width*4 for alignment padding; padding is skipped rather than
// treated as pixels.
func Decode(format Format, stride, width, height int, pixels []byte) (*image.RGBA, error) {
	switch format {
	case FormatARGB8888, FormatXRGB8888:
	default:
		return nil, fmt.Errorf("unsupported pixel format %s", format)
	}

	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid dimensions %dx%d", width, height)
	}
	rowBytes := width * 4
	if stride < rowBytes {
		return nil, fmt.Errorf("stride %d is smaller than a row of %d pixels (%d bytes)", stride, width, rowBytes)
	}
	if len(pixels) < stride*(height-1)+rowBytes {
		return nil, fmt.Errorf("buffer of %d bytes is too small for %dx%d at stride %d", len(pixels), width, height, stride)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		src := y * stride
		dst := y * img.Stride
		for x := 0; x < width; x++ {
			blue := pixels[src]
			green := pixels[src+1]
			red := pixels[src+2]
			alpha := pixels[src+3]

			if format == FormatXRGB8888 {
				alpha = 0xff
			}

			img.Pix[dst] = red
			img.Pix[dst+1] = green
			img.Pix[dst+2] = blue
			img.Pix[dst+3] = alpha

			src += 4
			dst += 4
		}
	}

	return img, nil
}

// Digest hashes the canonical pixels of an image.
//
// The digest covers the decoded RGBA bytes and the dimensions, so it is
// independent of the stride and the pixel format the compositor happened to
// use: two captures of the same picture hash the same.
func Digest(img *image.RGBA) [32]byte {
	hasher := sha256.New()
	var dimensions [8]byte
	bounds := img.Bounds()
	putUint32(dimensions[0:4], uint32(bounds.Dx()))
	putUint32(dimensions[4:8], uint32(bounds.Dy()))
	_, _ = hasher.Write(dimensions[:])

	// Hash row by row so the image's own stride never leaks into the digest.
	rowBytes := bounds.Dx() * 4
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		start := img.PixOffset(bounds.Min.X, y)
		_, _ = hasher.Write(img.Pix[start : start+rowBytes])
	}

	var digest [32]byte
	copy(digest[:], hasher.Sum(nil))
	return digest
}

// EncodePNG encodes an image losslessly with the standard library encoder.
func EncodePNG(img *image.RGBA) ([]byte, error) {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buffer.Bytes(), nil
}

func putUint32(dst []byte, value uint32) {
	dst[0] = byte(value >> 24)
	dst[1] = byte(value >> 16)
	dst[2] = byte(value >> 8)
	dst[3] = byte(value)
}
