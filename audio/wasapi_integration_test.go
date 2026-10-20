//go:build windows && integration

package audio

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"mfcapture/mf"
)

// TestWASAPICapturerSmoke 需要一个可用的默认渲染端点，用 integration tag 隔离。
// loopback 在系统静音时可能一个包都不产生，所以这里只要求能打开、能按契约
// 返回包或 ErrTimeout，不把"必须有声音"当作成功条件。
func TestWASAPICapturerSmoke(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if hr := mf.CoInitializeEx(); hr < 0 {
		t.Fatalf("CoInitializeEx: HRESULT 0x%08X", uint32(hr))
	}
	defer mf.CoUninitialize()

	c, err := NewWASAPICapturer(WASAPICapturerConfig{})
	if err != nil {
		t.Fatalf("NewWASAPICapturer: %v", err)
	}
	defer c.Release()

	format := c.Format()
	if format.SampleRate != OpusSampleRate {
		t.Errorf("sample rate=%d, want %d", format.SampleRate, OpusSampleRate)
	}
	if format.Channels != 1 && format.Channels != 2 {
		t.Errorf("channels=%d, want 1 or 2", format.Channels)
	}
	t.Logf("format: %d Hz, %d ch, float32=%v", format.SampleRate, format.Channels, format.Float32)

	deadline := time.Now().Add(500 * time.Millisecond)
	var packets, sampleFrames int
	var lastPTS time.Duration
	for time.Now().Before(deadline) {
		f, err := c.ReadFrame(20 * time.Millisecond)
		if errors.Is(err, ErrTimeout) {
			continue
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if f == nil {
			t.Fatal("ReadFrame returned a nil frame without an error")
		}

		samples := format.Samples(f.Data)
		if samples == 0 {
			t.Fatal("ReadFrame returned an empty packet")
		}
		if got, want := len(f.Data), samples*format.BlockAlign(); got != want {
			t.Fatalf("packet has %d bytes, want %d (%d sample frames)", got, want, samples)
		}
		if f.Duration <= 0 {
			t.Errorf("packet Duration=%v, want positive", f.Duration)
		}
		if packets > 0 && f.Timestamp < lastPTS {
			t.Errorf("PTS went backwards: %v after %v", f.Timestamp, lastPTS)
		}

		lastPTS = f.Timestamp
		sampleFrames += samples
		packets++
	}

	t.Logf("captured %d packets / %d sample frames in 500 ms (last PTS %v)",
		packets, sampleFrames, lastPTS)
}
