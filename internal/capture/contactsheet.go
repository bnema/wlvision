package capture

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
)

// Contact sheet geometry. A cell is big enough to identify a frame at a
// glance; nothing is scaled, so a reader can trust the pixels in a cell.
const (
	// SheetColumns is the default number of cells in a row.
	SheetColumns = 5
	// SheetCellWidth is one cell's width in pixels.
	SheetCellWidth = 320
	// SheetCellHeight is one cell's height in pixels.
	SheetCellHeight = 240
)

// Contact sheet colours. They are fixed so two sheets of the same frames are
// byte-for-byte alike.
var (
	sheetBackground  = color.RGBA{R: 0x18, G: 0x18, B: 0x20, A: 0xff}
	sheetPlaceholder = color.RGBA{R: 0x50, G: 0x14, B: 0x14, A: 0xff}
	sheetLabelColor  = color.RGBA{R: 0xf0, G: 0xf0, B: 0xf0, A: 0xff}

	sheetBackgroundUniform  = image.NewUniform(sheetBackground)
	sheetPlaceholderUniform = image.NewUniform(sheetPlaceholder)
	sheetLabelUniform       = image.NewUniform(sheetLabelColor)
)

// sheetLabelScale is how many pixels each font pixel covers.
const sheetLabelScale = 2

// SheetFrame is one cell of a contact sheet: its caption and its pixels. A
// frame with no readable pixels is drawn as a labelled placeholder.
type SheetFrame struct {
	Label string
	// PNG is a frame's encoded pixels. It is decoded when Image is nil, so a
	// caller that only has the payload can still build a sheet.
	PNG []byte
	// Image is a frame's pixels, already decoded. It takes precedence over
	// PNG, which lets a burst keep a cell-sized crop of each stored frame
	// instead of the whole payload, and lets a duplicated sample show the
	// frame that stored the picture.
	Image image.Image
}

// ContactSheet lays frames on a grid in the order given and returns it as an
// RGBA image.
//
// Each cell is drawn from the decoded PNG without scaling, into the cell's
// top-left corner, over a fixed background; the rest of the cell keeps that
// background. A frame that carries no readable PNG, such as one that was
// missed or failed, gets a labelled placeholder instead of a copy of a
// neighbour. columns <= 0 selects SheetColumns.
func ContactSheet(frames []SheetFrame, columns int) (*image.RGBA, error) {
	if columns <= 0 {
		columns = SheetColumns
	}
	if len(frames) == 0 {
		return nil, errors.New("capture: a contact sheet needs at least one frame")
	}

	rows := (len(frames) + columns - 1) / columns
	if rows < 1 {
		rows = 1
	}
	sheet := image.NewRGBA(image.Rect(0, 0, columns*SheetCellWidth, rows*SheetCellHeight))
	draw.Draw(sheet, sheet.Bounds(), sheetBackgroundUniform, image.Point{}, draw.Src)

	for index, frame := range frames {
		column := index % columns
		row := index / columns
		cell := image.Rect(column*SheetCellWidth, row*SheetCellHeight,
			(column+1)*SheetCellWidth, (row+1)*SheetCellHeight)
		drawSheetCell(sheet, cell, frame)
	}
	return sheet, nil
}

// drawSheetCell draws one frame, or its placeholder, into one cell.
func drawSheetCell(sheet *image.RGBA, cell image.Rectangle, frame SheetFrame) {
	pixels := frame.Image
	if pixels == nil {
		decoded, err := decodeSheetPNG(frame.PNG)
		if err != nil {
			draw.Draw(sheet, cell, sheetPlaceholderUniform, image.Point{}, draw.Src)
			drawSheetLabel(sheet, cell, frame.Label)
			return
		}
		pixels = decoded
	}

	draw.Draw(sheet, cell, sheetBackgroundUniform, image.Point{}, draw.Src)
	// The source's own origin maps onto the cell's origin, so the frame keeps
	// its orientation and is clipped to the cell rather than scaled.
	draw.Draw(sheet, cell, pixels, pixels.Bounds().Min, draw.Src)
	drawSheetLabel(sheet, cell, frame.Label)
}

// sheetCellImage decodes one stored frame and composes it into a single sheet
// cell: the frame's top-left corner over the sheet background, no scaling. A
// burst keeps this instead of the payload, so the memory it holds for a
// contact sheet is bounded by the sheet's size rather than by the frames it
// stored.
func sheetCellImage(data []byte) (*image.RGBA, error) {
	decoded, err := decodeSheetPNG(data)
	if err != nil {
		return nil, err
	}

	cell := image.NewRGBA(image.Rect(0, 0, SheetCellWidth, SheetCellHeight))
	draw.Draw(cell, cell.Bounds(), sheetBackgroundUniform, image.Point{}, draw.Src)
	draw.Draw(cell, cell.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return cell, nil
}

// decodeSheetPNG decodes a frame's PNG, treating anything unreadable as a
// missing frame.
func decodeSheetPNG(data []byte) (image.Image, error) {
	if len(data) == 0 {
		return nil, errors.New("capture: frame has no png")
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("capture: frame png: %w", err)
	}
	return decoded, nil
}

// drawSheetLabel renders a caption along the bottom of a cell, over a band of
// the sheet background so the text stays legible over any frame.
func drawSheetLabel(sheet *image.RGBA, cell image.Rectangle, label string) {
	if label == "" {
		return
	}

	lineHeight := 5 * sheetLabelScale
	band := image.Rect(cell.Min.X, cell.Max.Y-(lineHeight+4), cell.Max.X, cell.Max.Y)
	draw.Draw(sheet, band, sheetBackgroundUniform, image.Point{}, draw.Src)

	x := cell.Min.X + 4
	y := band.Min.Y + 2
	for _, glyph := range strings.ToUpper(label) {
		rows := sheetGlyph(glyph)
		for row := 0; row < 5; row++ {
			bits := rows[row]
			for column := 0; column < 3; column++ {
				if bits&(1<<(2-column)) == 0 {
					continue
				}
				pixel := image.Rect(
					x+column*sheetLabelScale, y+row*sheetLabelScale,
					x+column*sheetLabelScale+sheetLabelScale, y+row*sheetLabelScale+sheetLabelScale,
				).Intersect(cell)
				if pixel.Empty() {
					continue
				}
				draw.Draw(sheet, pixel, sheetLabelUniform, image.Point{}, draw.Src)
			}
		}
		x += 4 * sheetLabelScale
		if x+3*sheetLabelScale > cell.Max.X-4 {
			break
		}
	}
}

// sheetGlyph returns the five rows of a 3x5 bitmap glyph, most significant bit
// leftmost. The font is embedded here so a sheet needs no system font and
// renders identically everywhere; anything unmapped draws as a question mark.
// Lowercase is folded to uppercase by the caller.
func sheetGlyph(r rune) [5]uint8 {
	switch r {
	case '0':
		return [5]uint8{0b111, 0b101, 0b101, 0b101, 0b111}
	case '1':
		return [5]uint8{0b010, 0b110, 0b010, 0b010, 0b111}
	case '2':
		return [5]uint8{0b111, 0b001, 0b111, 0b100, 0b111}
	case '3':
		return [5]uint8{0b111, 0b001, 0b111, 0b001, 0b111}
	case '4':
		return [5]uint8{0b101, 0b101, 0b111, 0b001, 0b001}
	case '5':
		return [5]uint8{0b111, 0b100, 0b111, 0b001, 0b111}
	case '6':
		return [5]uint8{0b111, 0b100, 0b111, 0b101, 0b111}
	case '7':
		return [5]uint8{0b111, 0b001, 0b010, 0b010, 0b010}
	case '8':
		return [5]uint8{0b111, 0b101, 0b111, 0b101, 0b111}
	case '9':
		return [5]uint8{0b111, 0b101, 0b111, 0b001, 0b111}
	case 'A':
		return [5]uint8{0b010, 0b101, 0b111, 0b101, 0b101}
	case 'B':
		return [5]uint8{0b110, 0b101, 0b110, 0b101, 0b110}
	case 'C':
		return [5]uint8{0b011, 0b100, 0b100, 0b100, 0b011}
	case 'D':
		return [5]uint8{0b110, 0b101, 0b101, 0b101, 0b110}
	case 'E':
		return [5]uint8{0b111, 0b100, 0b110, 0b100, 0b111}
	case 'F':
		return [5]uint8{0b111, 0b100, 0b110, 0b100, 0b100}
	case 'G':
		return [5]uint8{0b011, 0b100, 0b101, 0b101, 0b011}
	case 'H':
		return [5]uint8{0b101, 0b101, 0b111, 0b101, 0b101}
	case 'I':
		return [5]uint8{0b111, 0b010, 0b010, 0b010, 0b111}
	case 'J':
		return [5]uint8{0b001, 0b001, 0b001, 0b101, 0b010}
	case 'K':
		return [5]uint8{0b101, 0b101, 0b110, 0b101, 0b101}
	case 'L':
		return [5]uint8{0b100, 0b100, 0b100, 0b100, 0b111}
	case 'M':
		return [5]uint8{0b101, 0b111, 0b111, 0b101, 0b101}
	case 'N':
		return [5]uint8{0b101, 0b111, 0b101, 0b101, 0b101}
	case 'O':
		return [5]uint8{0b010, 0b101, 0b101, 0b101, 0b010}
	case 'P':
		return [5]uint8{0b110, 0b101, 0b110, 0b100, 0b100}
	case 'Q':
		return [5]uint8{0b010, 0b101, 0b101, 0b110, 0b011}
	case 'R':
		return [5]uint8{0b110, 0b101, 0b110, 0b101, 0b101}
	case 'S':
		return [5]uint8{0b011, 0b100, 0b010, 0b001, 0b110}
	case 'T':
		return [5]uint8{0b111, 0b010, 0b010, 0b010, 0b010}
	case 'U':
		return [5]uint8{0b101, 0b101, 0b101, 0b101, 0b111}
	case 'V':
		return [5]uint8{0b101, 0b101, 0b101, 0b101, 0b010}
	case 'W':
		return [5]uint8{0b101, 0b101, 0b111, 0b111, 0b101}
	case 'X':
		return [5]uint8{0b101, 0b101, 0b010, 0b101, 0b101}
	case 'Y':
		return [5]uint8{0b101, 0b101, 0b010, 0b010, 0b010}
	case 'Z':
		return [5]uint8{0b111, 0b001, 0b010, 0b100, 0b111}
	case '-':
		return [5]uint8{0b000, 0b000, 0b111, 0b000, 0b000}
	case '.':
		return [5]uint8{0b000, 0b000, 0b000, 0b000, 0b010}
	case '/':
		return [5]uint8{0b001, 0b001, 0b010, 0b100, 0b100}
	case '_':
		return [5]uint8{0b000, 0b000, 0b000, 0b000, 0b111}
	case ':':
		return [5]uint8{0b000, 0b010, 0b000, 0b010, 0b000}
	case ' ':
		return [5]uint8{}
	default:
		return [5]uint8{0b110, 0b001, 0b010, 0b000, 0b010}
	}
}
