package mfcapture

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"mfcapture/capture"
	"mfcapture/encode"
)

// TestRecordZeroCopy is a manual integration test: it actually records the real local screen for
// 5 seconds. Run with: MFCAPTURE_MANUAL_TEST=1 go test -run TestRecordZeroCopy -v ./...
func TestRecordZeroCopy(t *testing.T) {
	if os.Getenv("MFCAPTURE_MANUAL_TEST") != "1" {
		t.Skip("set MFCAPTURE_MANUAL_TEST=1 to run this manual integration test")
	}

	out := t.TempDir() + "/mfcapture_test.mp4"
	cfg := Config{
		OutputPath: out,
		Duration:   5 * time.Second,
		BitrateBps: 8_000_000,
	}

	if err := Record(context.Background(), cfg); err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < 16 {
		t.Fatalf("output file too small: %d bytes", len(data))
	}
	if !containsFtyp(data[:64]) {
		t.Fatalf("output file does not look like an mp4 (no ftyp box found in header)")
	}
	t.Logf("recorded %d bytes to %s - open it in a player to verify", len(data), out)
}

// testSink 记录回调次数，用于集成测试。
type testSink struct {
	frames        atomic.Int64
	bytes         atomic.Int64
	cursorUpdates atomic.Int64
	gotKeyFrame   atomic.Bool
	desc          atomic.Pointer[VideoDesc]
	events        atomic.Int64
}

func (s *testSink) OnFrame(f *encode.EncodedFrame) {
	s.frames.Add(1)
	s.bytes.Add(int64(len(f.Data)))
	if f.IsKeyFrame {
		s.gotKeyFrame.Store(true)
	}
}

func (s *testSink) OnCursor(*capture.CursorInfo) { s.cursorUpdates.Add(1) }

func (s *testSink) OnEvent(e Event) {
	s.events.Add(1)
	if e.Kind == EventDescReady || e.Kind == EventResolutionChanged {
		d := e.Desc
		s.desc.Store(&d)
	}
}

// TestPipelineRun is a manual integration test: it runs a Pipeline for 3 seconds and verifies
// that an EventDescReady arrives before any frame, and that H264 frames are delivered.
// Run with: MFCAPTURE_MANUAL_TEST=1 go test -run TestPipelineRun -v ./...
func TestPipelineRun(t *testing.T) {
	if os.Getenv("MFCAPTURE_MANUAL_TEST") != "1" {
		t.Skip("set MFCAPTURE_MANUAL_TEST=1 to run this manual integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var sink testSink
	p, err := NewPipeline(PipelineConfig{
		BitrateBps: 8_000_000,
		MaxFPS:     30,
		LowLatency: true,
		GOPSize:    30,
	}, &sink)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	defer p.Close()

	if err := p.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	st := p.Stats()
	t.Logf("stats: %+v, cursor updates: %d", st, sink.cursorUpdates.Load())

	if st.Frames == 0 {
		t.Fatal("no encoded frames received")
	}
	if st.Bytes == 0 {
		t.Fatal("total encoded bytes is 0")
	}
	if !sink.gotKeyFrame.Load() {
		t.Fatal("no keyframe received")
	}
	if st.KeyFrames == 0 {
		t.Fatal("stats reported no keyframes")
	}
	d := sink.desc.Load()
	if d == nil || d.Width == 0 || d.Height == 0 {
		t.Fatalf("EventDescReady was not delivered with a valid resolution: %+v", d)
	}
	if d.Codec != "H264" {
		t.Fatalf("unexpected codec %q", d.Codec)
	}
	final := p.Desc()
	if final.Width != d.Width || final.Height != d.Height {
		t.Fatalf("Desc() = %dx%d, event said %dx%d", final.Width, final.Height, d.Width, d.Height)
	}
	// Profile/Level 是从流内 SPS 解析的，只有第一个关键帧到达后才填充，
	// 因此 EventDescReady 快照里必然是 0，这里要查的是当前 Desc()。
	if final.Profile == 0 || final.Level == 0 {
		t.Errorf("profile/level not parsed from in-band SPS (profile=%d level=%d)", final.Profile, final.Level)
	} else {
		t.Logf("parsed from SPS: profile_idc=%d level_idc=%d", final.Profile, final.Level)
	}
}

// TestEnumMonitors lists the captureable displays. Does not require COM/MF.
func TestEnumMonitors(t *testing.T) {
	monitors, err := capture.EnumMonitors()
	if err != nil {
		t.Fatalf("EnumMonitors: %v", err)
	}
	if len(monitors) == 0 {
		t.Fatal("no monitors found")
	}
	for i, m := range monitors {
		if m.Index != uint(i) {
			t.Fatalf("monitor %d has Index %d, want dense 0..n-1", i, m.Index)
		}
		if m.Width == 0 || m.Height == 0 {
			t.Errorf("monitor %d has invalid size %dx%d", i, m.Width, m.Height)
		}
		t.Logf("[%d] %s %dx%d at (%d,%d) primary=%v attached=%v",
			m.Index, m.Name, m.Width, m.Height, m.Left, m.Top, m.Primary, m.Attached)
	}
}

// TestParseSPSProfileLevel checks the SPS scanner used to fill VideoDesc.Profile/Level.
func TestParseSPSProfileLevel(t *testing.T) {
	// 00 00 00 01 | 67 | 4d(profile=77/Main) 40 32(level=50) ... | 00 00 00 01 65(IDR)
	annexB := []byte{
		0x00, 0x00, 0x00, 0x01, 0x09, 0x10,
		0x00, 0x00, 0x00, 0x01, 0x67, 0x4d, 0x40, 0x32, 0x95, 0xa0,
		0x00, 0x00, 0x00, 0x01, 0x68, 0xee, 0x3c, 0x80,
		0x00, 0x00, 0x00, 0x01, 0x65, 0xb8, 0x04, 0x37,
	}
	profile, level, ok := parseSPSProfileLevel(annexB)
	if !ok {
		t.Fatal("SPS not found")
	}
	if profile != 77 {
		t.Errorf("profile = %d, want 77 (Main)", profile)
	}
	if level != 50 {
		t.Errorf("level = %d, want 50", level)
	}
	if _, _, ok := parseSPSProfileLevel([]byte{0x00, 0x00, 0x01, 0x65, 0x00}); ok {
		t.Error("should not report SPS for a stream with only an IDR slice")
	}
}

func containsFtyp(b []byte) bool {
	for i := 0; i+4 <= len(b); i++ {
		if b[i] == 'f' && b[i+1] == 't' && b[i+2] == 'y' && b[i+3] == 'p' {
			return true
		}
	}
	return false
}
