package capture

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"
)

// solidPNG encodes a single-colour frame of the given size.
func solidPNG(t *testing.T, width, height int, fill color.RGBA) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(fill), image.Point{}, draw.Src)
	encoded, err := EncodePNG(img)
	if err != nil {
		t.Fatalf("encoding a frame: %v", err)
	}
	return encoded
}

// cell returns the rectangle of one cell of a sheet.
func cell(index, columns int) image.Rectangle {
	column := index % columns
	row := index / columns
	return image.Rect(column*SheetCellWidth, row*SheetCellHeight,
		(column+1)*SheetCellWidth, (row+1)*SheetCellHeight)
}

// countColor counts the pixels of one colour inside an area.
func countColor(img *image.RGBA, area image.Rectangle, want color.RGBA) int {
	count := 0
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			if img.RGBAAt(x, y) == want {
				count++
			}
		}
	}
	return count
}

func TestContactSheetLaysFramesInOrder(t *testing.T) {
	frames := make([]SheetFrame, 7)
	colors := make([]color.RGBA, len(frames))
	for index := range frames {
		colors[index] = color.RGBA{R: uint8(index*10 + 1), A: 0xff}
		frames[index] = SheetFrame{Label: fmt.Sprintf("frame %d", index), PNG: solidPNG(t, 4, 4, colors[index])}
	}

	sheet, err := ContactSheet(frames, 0)
	if err != nil {
		t.Fatalf("ContactSheet returned %v", err)
	}
	if got, want := sheet.Bounds(), image.Rect(0, 0, 5*SheetCellWidth, 2*SheetCellHeight); got != want {
		t.Fatalf("sheet bounds %v, want %v", got, want)
	}

	for index := range frames {
		area := cell(index, SheetColumns)
		if got := sheet.RGBAAt(area.Min.X, area.Min.Y); got != colors[index] {
			t.Errorf("cell %d starts with %v, want the frame's %v", index, got, colors[index])
		}
		// The frame is four pixels wide, so the rest of the cell keeps the
		// sheet background instead of stretching the picture.
		if got := sheet.RGBAAt(area.Min.X+10, area.Min.Y+10); got != sheetBackground {
			t.Errorf("cell %d padding is %v, want the sheet background %v", index, got, sheetBackground)
		}
		// The label is present on every cell.
		if countColor(sheet, area, sheetLabelColor) == 0 {
			t.Errorf("cell %d carries no label", index)
		}
	}
}

func TestContactSheetPlacesMissingFramesAsLabelledPlaceholders(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}
	frames := []SheetFrame{
		{Label: "01 captured", PNG: solidPNG(t, 4, 4, red)},
		{Label: "02 missed"},
		{Label: "03 failed", PNG: []byte("this is not a png")},
	}

	sheet, err := ContactSheet(frames, 3)
	if err != nil {
		t.Fatalf("ContactSheet returned %v", err)
	}
	if got, want := sheet.Bounds(), image.Rect(0, 0, 3*SheetCellWidth, SheetCellHeight); got != want {
		t.Fatalf("sheet bounds %v, want %v", got, want)
	}

	if got := sheet.RGBAAt(0, 0); got != red {
		t.Errorf("the readable frame's cell starts with %v, want %v", got, red)
	}
	for index := 1; index < len(frames); index++ {
		area := cell(index, 3)
		if got := sheet.RGBAAt(area.Min.X, area.Min.Y); got != sheetPlaceholder {
			t.Errorf("cell %d starts with %v, want the placeholder %v", index, got, sheetPlaceholder)
		}
		if countColor(sheet, area, red) != 0 {
			t.Errorf("cell %d copied a neighbouring frame", index)
		}
		if countColor(sheet, area, sheetLabelColor) == 0 {
			t.Errorf("cell %d carries no label", index)
		}
	}
}

func TestContactSheetHonoursTheColumnCount(t *testing.T) {
	middle := color.RGBA{G: 0x80, A: 0xff}
	frames := []SheetFrame{
		{PNG: solidPNG(t, 2, 2, color.RGBA{R: 0x10, A: 0xff})},
		{PNG: solidPNG(t, 2, 2, middle)},
		{PNG: solidPNG(t, 2, 2, color.RGBA{B: 0x10, A: 0xff})},
	}

	sheet, err := ContactSheet(frames, 2)
	if err != nil {
		t.Fatalf("ContactSheet returned %v", err)
	}
	if got, want := sheet.Bounds(), image.Rect(0, 0, 2*SheetCellWidth, 2*SheetCellHeight); got != want {
		t.Fatalf("sheet bounds %v, want %v", got, want)
	}
	// The third frame wraps onto the second row's first cell.
	third := cell(2, 2)
	if got := sheet.RGBAAt(third.Min.X, third.Min.Y); got != (color.RGBA{B: 0x10, A: 0xff}) {
		t.Errorf("the wrapped frame drew %v, want its own colour", got)
	}
}

func TestContactSheetDoesNotScaleSmallFrames(t *testing.T) {
	fill := color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff}
	sheet, err := ContactSheet([]SheetFrame{{PNG: solidPNG(t, 8, 6, fill)}}, 1)
	if err != nil {
		t.Fatalf("ContactSheet returned %v", err)
	}
	if got := sheet.RGBAAt(7, 5); got != fill {
		t.Errorf("the frame's last pixel is %v, want %v", got, fill)
	}
	if got := sheet.RGBAAt(8, 5); got != sheetBackground {
		t.Errorf("the pixel past the frame is %v, want the background %v", got, sheetBackground)
	}
	if got := sheet.RGBAAt(7, 6); got != sheetBackground {
		t.Errorf("the row past the frame is %v, want the background %v", got, sheetBackground)
	}
}

// mustEncodePNG encodes an image a test built by hand.
func mustEncodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()

	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("encoding a frame: %v", err)
	}
	return buffer.Bytes()
}

func TestContactSheetDrawsADecodedImageWithoutAPng(t *testing.T) {
	fill := color.RGBA{G: 0x33, A: 0xff}
	pixels := image.NewRGBA(image.Rect(0, 0, 6, 6))
	draw.Draw(pixels, pixels.Bounds(), image.NewUniform(fill), image.Point{}, draw.Src)

	sheet, err := ContactSheet([]SheetFrame{
		{Label: "s01 captured", Image: pixels},
		{Label: "s02 missed"},
	}, 0)
	if err != nil {
		t.Fatalf("ContactSheet returned %v", err)
	}
	if got := sheet.RGBAAt(0, 0); got != fill {
		t.Errorf("the cell carrying an image starts with %v, want %v", got, fill)
	}
	if got := sheet.RGBAAt(10, 10); got != sheetBackground {
		t.Errorf("the cell padding is %v, want the background %v", got, sheetBackground)
	}
	if got := sheet.RGBAAt(SheetCellWidth, 0); got != sheetPlaceholder {
		t.Errorf("the missing frame's cell starts with %v, want the placeholder %v", got, sheetPlaceholder)
	}
}

func TestSheetCellImageCropsRatherThanScales(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}
	blue := color.RGBA{B: 0xff, A: 0xff}
	frame := image.NewRGBA(image.Rect(0, 0, 2*SheetCellWidth, 2*SheetCellHeight))
	draw.Draw(frame, image.Rect(0, 0, SheetCellWidth, 2*SheetCellHeight), image.NewUniform(red), image.Point{}, draw.Src)
	draw.Draw(frame, image.Rect(SheetCellWidth, 0, 2*SheetCellWidth, 2*SheetCellHeight), image.NewUniform(blue), image.Point{}, draw.Src)

	cell, err := sheetCellImage(mustEncodePNG(t, frame))
	if err != nil {
		t.Fatalf("sheetCellImage returned %v", err)
	}
	if got, want := cell.Bounds(), image.Rect(0, 0, SheetCellWidth, SheetCellHeight); got != want {
		t.Fatalf("cell bounds %v, want one cell %v", got, want)
	}
	if got, want := countColor(cell, cell.Bounds(), red), SheetCellWidth*SheetCellHeight; got != want {
		t.Errorf("the cell holds %d of %d red pixels, want the frame's top-left crop unscaled", got, want)
	}
	if got := countColor(cell, cell.Bounds(), blue); got != 0 {
		t.Errorf("the cell holds %d blue pixels, so the part of the frame beyond the crop was kept or scaled", got)
	}
}

func TestContactSheetRefusesNoFrames(t *testing.T) {
	if _, err := ContactSheet(nil, SheetColumns); err == nil {
		t.Fatal("ContactSheet accepted an empty frame list")
	}
}

func TestContactSheetIsAValidPNG(t *testing.T) {
	sheet, err := ContactSheet([]SheetFrame{{Label: "x", PNG: solidPNG(t, 2, 2, color.RGBA{A: 0xff})}}, 0)
	if err != nil {
		t.Fatalf("ContactSheet returned %v", err)
	}
	encoded, err := EncodePNG(sheet)
	if err != nil {
		t.Fatalf("encoding the sheet: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(encoded)); err != nil {
		t.Fatalf("the sheet is not a decodable png: %v", err)
	}
}
