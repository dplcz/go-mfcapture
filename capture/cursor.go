package capture

import (
	"context"
	"image"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// CursorCallback is called when cursor state changes (position, visibility, or shape).
type CursorCallback func(info *CursorInfo)

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	gdi32               = syscall.NewLazyDLL("gdi32.dll")
	pGetCursorInfo      = user32.NewProc("GetCursorInfo")
	pGetIconInfo        = user32.NewProc("GetIconInfo")
	pGetDIBits          = gdi32.NewProc("GetDIBits")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pGetDC              = user32.NewProc("GetDC")
	pReleaseDC          = user32.NewProc("ReleaseDC")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pGetBitmapInfo      = gdi32.NewProc("GetObjectW")
)

type cursorInfoW struct {
	cbSize      uint32
	flags       uint32
	hCursor     uintptr
	ptScreenPos point32
}

type point32 struct {
	X, Y int32
}

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type bitmap struct {
	bmType       int32
	bmWidth      int32
	bmHeight     int32
	bmWidthBytes int32
	bmPlanes     uint16
	bmBitsPixel  uint16
	bmBits       uintptr
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

const (
	cursorShowing  = 0x00000001
	dib_RGB_COLORS = 0
	bi_RGB         = 0
)

// CursorMonitor polls the system cursor via Win32 API at a fixed rate,
// independent of DXGI frame capture. It caches cursor shapes and only
// sends the full bitmap on the first occurrence of each HCURSOR value.
type CursorMonitor struct {
	cb       CursorCallback
	interval time.Duration
	once     sync.Once
	cancel   context.CancelFunc

	sentShapes  map[uintptr]struct{}
	lastCursor  uintptr
	lastPos     point32
	lastVisible bool
}

// NewCursorMonitor creates a cursor monitor that fires cb on every change.
// pollHz controls the polling rate (0 = 30 Hz default).
func NewCursorMonitor(cb CursorCallback, pollHz uint32) *CursorMonitor {
	if pollHz == 0 {
		pollHz = 30
	}
	return &CursorMonitor{
		cb:         cb,
		interval:   time.Second / time.Duration(pollHz),
		sentShapes: make(map[uintptr]struct{}),
	}
}

// Start begins polling in a new goroutine. Blocks until ctx is cancelled.
func (m *CursorMonitor) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll()
		}
	}
}

// Stop cancels the polling goroutine.
func (m *CursorMonitor) Stop() {
	m.once.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
	})
}

func (m *CursorMonitor) poll() {
	var ci cursorInfoW
	ci.cbSize = uint32(unsafe.Sizeof(ci))
	ret, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if ret == 0 {
		return
	}

	visible := ci.flags&cursorShowing != 0
	posChanged := ci.ptScreenPos != m.lastPos || visible != m.lastVisible
	shapeChanged := ci.hCursor != m.lastCursor

	if !posChanged && !shapeChanged {
		return
	}

	m.lastPos = ci.ptScreenPos
	m.lastVisible = visible
	m.lastCursor = ci.hCursor

	info := &CursorInfo{
		Position: image.Pt(int(ci.ptScreenPos.X), int(ci.ptScreenPos.Y)),
		Visible:  visible,
		ID:       uint64(ci.hCursor),
	}

	if shapeChanged {
		if _, sent := m.sentShapes[ci.hCursor]; !sent {
			if shape, w, h, hx, hy, ok := extractCursorBitmap(ci.hCursor); ok {
				info.IsNew = true
				info.Shape = shape
				info.Width = w
				info.Height = h
				info.HotSpot = image.Pt(hx, hy)
				m.sentShapes[ci.hCursor] = struct{}{}
			}
		}
	}

	m.cb(info)
}

func extractCursorBitmap(hCursor uintptr) (shape []byte, w, h, hotX, hotY int, ok bool) {
	var ii iconInfo
	ret, _, _ := pGetIconInfo.Call(hCursor, uintptr(unsafe.Pointer(&ii)))
	if ret == 0 {
		return
	}
	defer func() {
		if ii.hbmColor != 0 {
			pDeleteObject.Call(ii.hbmColor)
		}
		if ii.hbmMask != 0 {
			pDeleteObject.Call(ii.hbmMask)
		}
	}()

	hotX = int(ii.xHotspot)
	hotY = int(ii.yHotspot)

	hbm := ii.hbmColor
	if hbm == 0 {
		hbm = ii.hbmMask
	}

	var bm bitmap
	ret, _, _ = pGetBitmapInfo.Call(hbm, uintptr(unsafe.Sizeof(bm)), uintptr(unsafe.Pointer(&bm)))
	if ret == 0 || bm.bmWidth == 0 || bm.bmHeight == 0 {
		return
	}

	w = int(bm.bmWidth)
	h = int(bm.bmHeight)

	// For monochrome cursors (no hbmColor), the mask bitmap is double-height:
	// top half = AND mask, bottom half = XOR mask. We use only the visible height.
	if ii.hbmColor == 0 {
		h = h / 2
	}

	hdc, _, _ := pGetDC.Call(0)
	if hdc == 0 {
		return
	}
	defer pReleaseDC.Call(0, hdc)

	memDC, _, _ := pCreateCompatibleDC.Call(hdc)
	if memDC == 0 {
		return
	}
	defer pDeleteDC.Call(memDC)

	var bih bitmapInfoHeader
	bih.biSize = uint32(unsafe.Sizeof(bih))
	bih.biWidth = int32(w)
	bih.biHeight = -int32(h) // top-down
	bih.biPlanes = 1
	bih.biBitCount = 32
	bih.biCompression = bi_RGB

	shape = make([]byte, w*h*4)
	ret, _, _ = pGetDIBits.Call(
		memDC,
		hbm,
		0,
		uintptr(h),
		uintptr(unsafe.Pointer(&shape[0])),
		uintptr(unsafe.Pointer(&bih)),
		dib_RGB_COLORS,
	)
	if ret == 0 {
		return nil, 0, 0, 0, 0, false
	}

	ok = true
	return
}
