package capture

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// pixel builds one little-endian ARGB8888/XRGB8888 word as the compositor
// stores it: blue, green, red, alpha in memory order.
func pixel(red, green, blue, alpha byte) [4]byte {
	return [4]byte{blue, green, red, alpha}
}

func TestDecodeSwizzlesChannels(t *testing.T) {
	pixels := make([]byte, 0, 8)
	first := pixel(0x11, 0x22, 0x33, 0x44)
	second := pixel(0xaa, 0xbb, 0xcc, 0xdd)
	pixels = append(pixels, first[:]...)
	pixels = append(pixels, second[:]...)

	img, err := Decode(FormatARGB8888, 8, 2, 1, pixels)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	want := []color.RGBA{
		{R: 0x11, G: 0x22, B: 0x33, A: 0x44},
		{R: 0xaa, G: 0xbb, B: 0xcc, A: 0xdd},
	}
	for i, expected := range want {
		if got := img.RGBAAt(i, 0); got != expected {
			t.Errorf("pixel %d = %+v, want %+v", i, got, expected)
		}
	}
}

// XRGB8888 has no alpha channel; the result must be fully opaque rather than
// transparent because the unused byte happens to be zero.
func TestDecodeXRGBForcesOpacity(t *testing.T) {
	value := pixel(0x10, 0x20, 0x30, 0x00)

	img, err := Decode(FormatXRGB8888, 4, 1, 1, value[:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := img.RGBAAt(0, 0); got != (color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff}) {
		t.Fatalf("pixel = %+v, want opaque", got)
	}
}

// Padding beyond the last pixel of a row is alignment, not pixels, and must not
// bleed into the next row.
func TestDecodeHonoursStridePadding(t *testing.T) {
	const (
		width  = 2
		height = 2
		stride = 12 // two pixels plus four bytes of padding per row
	)

	pixels := make([]byte, stride*height)
	rows := [][]byte{
		{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
	}
	for y, row := range rows {
		copy(pixels[y*stride:], row)
		// Padding bytes are garbage on purpose.
		for i := 8; i < stride; i++ {
			pixels[y*stride+i] = 0xff
		}
	}

	img, err := Decode(FormatARGB8888, stride, width, height, pixels)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got := img.RGBAAt(0, 0); got != (color.RGBA{R: 0x03, G: 0x02, B: 0x01, A: 0x04}) {
		t.Errorf("pixel (0,0) = %+v", got)
	}
	if got := img.RGBAAt(1, 1); got != (color.RGBA{R: 0x17, G: 0x16, B: 0x15, A: 0x18}) {
		t.Errorf("pixel (1,1) = %+v, want the second row, not padding", got)
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		format Format
		stride int
		width  int
		height int
		buffer int
	}{
		{name: "unknown format", format: Format(0xdeadbeef), stride: 4, width: 1, height: 1, buffer: 4},
		{name: "zero width", format: FormatARGB8888, stride: 4, width: 0, height: 1, buffer: 4},
		{name: "stride below a row", format: FormatARGB8888, stride: 2, width: 1, height: 1, buffer: 4},
		{name: "truncated buffer", format: FormatARGB8888, stride: 8, width: 2, height: 2, buffer: 12},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.format, tc.stride, tc.width, tc.height, make([]byte, tc.buffer)); err == nil {
				t.Fatal("Decode accepted invalid input")
			}
		})
	}
}

// The digest identifies the picture, not the buffer layout: the same pixels at
// two different strides must hash identically.
func TestDigestIgnoresStride(t *testing.T) {
	const (
		width  = 2
		height = 2
	)

	tight := make([]byte, 4*width*height)
	padded := make([]byte, 12*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width*4; x++ {
			value := byte(y*width*4 + x)
			tight[y*width*4+x] = value
			padded[y*12+x] = value
		}
	}

	tightImage, err := Decode(FormatARGB8888, width*4, width, height, tight)
	if err != nil {
		t.Fatalf("Decode(tight): %v", err)
	}
	paddedImage, err := Decode(FormatARGB8888, 12, width, height, padded)
	if err != nil {
		t.Fatalf("Decode(padded): %v", err)
	}

	if Digest(tightImage) != Digest(paddedImage) {
		t.Fatal("digest depends on stride; identical pictures must hash the same")
	}
}

func TestDigestDistinguishesPictures(t *testing.T) {
	first, err := Decode(FormatARGB8888, 8, 2, 1, []byte{0, 0, 0, 0, 1, 1, 1, 1})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	second, err := Decode(FormatARGB8888, 8, 2, 1, []byte{0, 0, 0, 0, 2, 2, 2, 2})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if Digest(first) == Digest(second) {
		t.Fatal("different pictures hash the same")
	}
}

func TestEncodePNGRoundTrips(t *testing.T) {
	pixels := []byte{0x33, 0x22, 0x11, 0xff}
	img, err := Decode(FormatARGB8888, 4, 1, 1, pixels)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	encoded, err := EncodePNG(img)
	if err != nil {
		t.Fatalf("EncodePNG: %v", err)
	}

	decoded, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	got := color.RGBAModel.Convert(decoded.At(0, 0)).(color.RGBA)
	if got != (color.RGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xff}) {
		t.Fatalf("round-tripped pixel = %+v", got)
	}
	if decoded.Bounds() != image.Rect(0, 0, 1, 1) {
		t.Fatalf("round-tripped bounds = %v", decoded.Bounds())
	}
}
