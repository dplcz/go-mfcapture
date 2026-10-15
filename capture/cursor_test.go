package capture

import (
	"testing"
)

// px builds a 32bpp pixel (B, G, R, A).
func px(b, g, r, a byte) []byte { return []byte{b, g, r, a} }

func pixels(ps ...[]byte) []byte {
	var out []byte
	for _, p := range ps {
		out = append(out, p...)
	}
	return out
}

var (
	black = px(0, 0, 0, 0)
	white = px(0xFF, 0xFF, 0xFF, 0)
)

func expectPixels(t *testing.T, got []byte, want ...[]byte) {
	t.Helper()
	w := pixels(want...)
	if len(got) != len(w) {
		t.Fatalf("got %d bytes, want %d", len(got), len(w))
	}
	for i := 0; i < len(w); i += 4 {
		if string(got[i:i+4]) != string(w[i:i+4]) {
			t.Errorf("pixel %d = %v, want %v", i/4, got[i:i+4], w[i:i+4])
		}
	}
}

func TestCursorToBGRAAlpha(t *testing.T) {
	// 32bpp cursor with alpha: passed through untouched (straight alpha), mask ignored.
	color := pixels(px(10, 20, 30, 0x80), px(0, 0, 0, 0))
	mask := pixels(white, white)
	expectPixels(t, cursorToBGRA(color, mask, 2, 1), px(10, 20, 30, 0x80), px(0, 0, 0, 0))
}

func TestCursorToBGRAColorNoAlpha(t *testing.T) {
	// AND=0 → opaque color; AND=1 & black → transparent; AND=1 & color → inverted.
	// Row 0: opaque, transparent, transparent; row 1: transparent, transparent, invert.
	// Transparent pixels touching the inverted one become the white outline.
	color := pixels(
		px(1, 2, 3, 0), black, black,
		black, black, px(9, 9, 9, 0),
	)
	mask := pixels(
		black, white, white,
		white, white, white,
	)
	expectPixels(t, cursorToBGRA(color, mask, 3, 2),
		px(1, 2, 3, 0xFF), px(0xFF, 0xFF, 0xFF, 0xFF), px(0xFF, 0xFF, 0xFF, 0xFF),
		px(0, 0, 0, 0), px(0xFF, 0xFF, 0xFF, 0xFF), px(0, 0, 0, 0xFF),
	)
}

func TestCursorToBGRAMonochrome(t *testing.T) {
	// AND/XOR: 00 black, 01 white, 10 transparent, 11 invert.
	and := pixels(black, black, white, white)
	xor := pixels(black, white, black, white)
	got := cursorToBGRA(nil, pixels(and, xor), 4, 1)
	expectPixels(t, got,
		px(0, 0, 0, 0xFF), px(0xFF, 0xFF, 0xFF, 0xFF),
		px(0xFF, 0xFF, 0xFF, 0xFF), // transparent, but next to the inverted pixel: outline
		px(0, 0, 0, 0xFF),          // inverted → black
	)
}

// An all-invert monochrome cursor (like the I-beam) becomes black with a white outline.
func TestCursorToBGRAInvertOutline(t *testing.T) {
	const w, h = 5, 5
	and := make([]byte, w*h*4)
	for i := range and {
		and[i] = 0xFF // all transparent...
	}
	xor := make([]byte, w*h*4)
	for i := range 4 {
		xor[(2*w+2)*4+i] = 0xFF // ...except the center, which inverts
	}
	got := cursorToBGRA(nil, pixels(and, xor), w, h)
	for y := range h {
		for x := range w {
			p := got[(y*w+x)*4:][:4]
			switch dx, dy := x-2, y-2; {
			case dx == 0 && dy == 0:
				expectPixels(t, p, px(0, 0, 0, 0xFF))
			case dx >= -1 && dx <= 1 && dy >= -1 && dy <= 1:
				expectPixels(t, p, px(0xFF, 0xFF, 0xFF, 0xFF))
			default:
				expectPixels(t, p, px(0, 0, 0, 0))
			}
		}
	}
}

// System cursors cover all paths: the arrow is 32bpp with alpha, the I-beam and
// crosshair are monochrome inverting cursors.
func TestExtractSystemCursors(t *testing.T) {
	load := user32.NewProc("LoadCursorW")
	for _, c := range []struct {
		name string
		id   uintptr
	}{{"arrow", 32512}, {"ibeam", 32513}, {"cross", 32515}, {"hand", 32649}} {
		h, _, _ := load.Call(0, c.id)
		if h == 0 {
			t.Fatalf("LoadCursor(%s) failed", c.name)
		}
		shape, w, hh, _, _, ok := extractCursorBitmap(h)
		if !ok {
			t.Fatalf("%s: extraction failed", c.name)
		}
		if len(shape) != w*hh*4 {
			t.Fatalf("%s: %d bytes for %dx%d", c.name, len(shape), w, hh)
		}
		var opaque, transparent int
		for i := 3; i < len(shape); i += 4 {
			switch shape[i] {
			case 0xFF:
				opaque++
			case 0:
				transparent++
			}
		}
		t.Logf("%s: %dx%d opaque=%d transparent=%d", c.name, w, hh, opaque, transparent)
		if opaque == 0 || transparent == 0 {
			t.Errorf("%s: %d opaque / %d transparent pixels: shape lost its mask", c.name, opaque, transparent)
		}
	}
}
