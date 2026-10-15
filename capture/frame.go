package capture

import (
	"image"
	"time"

	"github.com/kirides/go-d3d/d3d11"
)

// CapturedFrame holds one acquired desktop frame.
type CapturedFrame struct {
	Texture   *d3d11.ID3D11Texture2D
	Timestamp time.Duration
}

// CursorInfo holds cursor state delivered independently from video frames.
// When IsNew is true, Shape contains the BGRA pixel data for a newly seen
// cursor shape. When IsNew is false, the shape was already sent (look up by ID).
type CursorInfo struct {
	Position image.Point
	Visible  bool
	ID       uint64 // HCURSOR handle value, used as cache key
	IsNew    bool   // true = first occurrence of this shape
	// Populated only when IsNew == true
	Shape   []byte // BGRA pixel data (top-down, straight alpha; see cursorToBGRA)
	Width   int
	Height  int
	HotSpot image.Point
}
