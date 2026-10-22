//go:build windows && integration

package audio

import (
	"math"
	"runtime"
	"testing"
	"time"

	"mfcapture/mf"
)

// TestWASAPIRendererSmoke 需要一台带默认输出端点的机器，用 integration tag
// 隔离。会真的响一秒钟 440 Hz 单音。断言的是"数据被设备实时消费"而不是听感：
// 1 秒 PCM 写入 + Drain 的时间必须接近 1 秒（共享模式缓冲只有几十毫秒，
// 不可能一次吞下整秒）。
func TestWASAPIRendererSmoke(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if hr := mf.CoInitializeEx(); hr < 0 {
		t.Fatalf("CoInitializeEx: HRESULT 0x%08X", uint32(hr))
	}
	defer mf.CoUninitialize()

	r, err := NewWASAPIRenderer(WASAPIRendererConfig{Channels: 2})
	if err != nil {
		t.Fatalf("NewWASAPIRenderer: %v", err)
	}
	defer r.Release()

	format := r.Format()
	if format.SampleRate != OpusSampleRate {
		t.Errorf("sample rate=%d, want %d", format.SampleRate, OpusSampleRate)
	}
	if format.Channels != 1 && format.Channels != 2 {
		t.Errorf("channels=%d, want 1 or 2", format.Channels)
	}
	t.Logf("format: %d Hz, %d ch, float32=%v", format.SampleRate, format.Channels, format.Float32)

	const seconds = 1.0
	frames := int(OpusSampleRate * seconds)
	pcm := make([]float32, frames*2)
	for i := range frames {
		v := float32(0.15 * math.Sin(2*math.Pi*440*float64(i)/OpusSampleRate))
		pcm[i*2] = v
		pcm[i*2+1] = v
	}

	start := time.Now()
	if err := r.Write(pcm); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := r.Drain(3 * time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 500*time.Millisecond {
		t.Errorf("1 s of PCM was accepted in %v; the renderer is not paced by the device", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Write+Drain took %v for 1 s of audio", elapsed)
	}
	t.Logf("played 1 s in %v", elapsed)
}
