package audio

import (
	"math"
	"testing"
	"time"
)

// genTone 生成 freq Hz 的交错正弦，幅度 amp，长度 samples 帧（每声道）。
//
// startSample 是这段信号在整个流里的起始位置，用来让跨帧生成的正弦保持连续
// 相位：每帧都从相位 0 重新开始会在帧边界引入咔哒声，那是宽带信号，会明显
// 拉低 Opus 的编码质量，让往返测试测到的是测试素材的问题而不是编解码的问题。
func genTone(startSample, samples, channels int, freq float64, amp float32) []float32 {
	pcm := make([]float32, samples*channels)
	for i := 0; i < samples; i++ {
		v := float32(math.Sin(2*math.Pi*freq*float64(startSample+i)/float64(OpusSampleRate))) * amp
		for c := 0; c < channels; c++ {
			pcm[i*channels+c] = v
		}
	}
	return pcm
}

func mustEncoder(t *testing.T, cfg OpusEncoderConfig) *OpusEncoder {
	t.Helper()
	enc, err := NewOpusEncoder(cfg)
	if err != nil {
		t.Fatalf("NewOpusEncoder(%+v): %v", cfg, err)
	}
	t.Cleanup(enc.Release)
	return enc
}

func TestNewOpusEncoderDefaults(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{})
	if got := enc.SampleRate(); got != OpusSampleRate {
		t.Errorf("SampleRate()=%d, want %d", got, OpusSampleRate)
	}
	if got := enc.Channels(); got != 2 {
		t.Errorf("Channels()=%d, want 2", got)
	}
	if got := enc.FrameSamples(); got != 960 {
		t.Errorf("FrameSamples()=%d, want 960", got)
	}
}

// TestOpusPreSkipMatchesLibrary 守住 defaultPreSkip 这个兜底常量：它是解码器
// 在拿不到对端 AudioDesc 时的默认值，一旦 go-opus 改了算法延迟（换版本、换
// 实现），这条测试必须先失败，而不是让音频静默错位 6.5 ms。
func TestOpusPreSkipMatchesLibrary(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{})
	got := enc.PreSkip()
	if got != defaultPreSkip {
		t.Errorf("OpusEncoder.PreSkip()=%d, decoder default constant defaultPreSkip=%d; "+
			"update the constant to keep the fallback honest", got, defaultPreSkip)
	}
	if got <= 0 || got >= frameSamples {
		t.Errorf("PreSkip()=%d, want a delay smaller than one %d-sample frame", got, frameSamples)
	}
}

func TestNewOpusEncoderRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  OpusEncoderConfig
	}{
		{"sample rate", OpusEncoderConfig{SampleRate: 44100}},
		{"channels", OpusEncoderConfig{Channels: 3}},
		{"complexity", OpusEncoderConfig{Complexity: 11}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewOpusEncoder(c.cfg); err == nil {
				t.Fatalf("NewOpusEncoder(%+v) succeeded, want error", c.cfg)
			}
		})
	}
}

// TestOpusEncoderRejectsNonFrameLength 是适配器的核心契约：Opus 只接受固定
// 帧长，分帧是采集层的事，长度不对必须报错而不是静默补零或截断。
func TestOpusEncoderRejectsNonFrameLength(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 2})
	want := enc.FrameSamples() * 2
	for _, n := range []int{0, 1, 959, 961, 2 * want} {
		if _, err := enc.Encode(make([]float32, n), 0); err == nil {
			t.Errorf("Encode(len=%d) succeeded, want error (want len=%d)", n, want)
		}
	}
}

func TestOpusEncoderPacketSizes(t *testing.T) {
	const bitrate = 128_000
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 2, BitrateBps: bitrate, CBR: true})

	var total int
	const frames = 50
	for i := 0; i < frames; i++ {
		f, err := enc.Encode(genTone(i*enc.FrameSamples(), enc.FrameSamples(), 2, 440, 0.5), 0)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if len(f.Data) == 0 {
			t.Fatalf("frame %d: empty packet", i)
		}
		if len(f.Data) > maxPacketSize {
			t.Fatalf("frame %d: packet %d bytes exceeds %d", i, len(f.Data), maxPacketSize)
		}
		total += len(f.Data)
	}

	// 20 ms × 128 kbps = 320 字节。CBR 下每包应贴近这个值；给 ±25% 的窗口
	// 吸收首包的头开销与 CELT 的帧内波动。
	wantBytes := bitrate * OpusFrameMs / 1000 / 8
	avg := float64(total) / frames
	if avg < float64(wantBytes)*0.75 || avg > float64(wantBytes)*1.25 {
		t.Errorf("average packet %0.1f bytes, want ~%d (±25%%)", avg, wantBytes)
	}
}

func TestOpusEncoderTimestamps(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 1})
	var ts time.Duration
	const frames = 50
	for i := 0; i < frames; i++ {
		f, err := enc.Encode(genTone(i*enc.FrameSamples(), enc.FrameSamples(), 1, 440, 0.5), ts)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		want := time.Duration(i) * OpusFrameMs * time.Millisecond
		if f.Timestamp != want {
			t.Fatalf("frame %d: Timestamp=%v, want %v", i, f.Timestamp, want)
		}
		if f.Duration != OpusFrameMs*time.Millisecond {
			t.Fatalf("frame %d: Duration=%v, want %v", i, f.Duration, OpusFrameMs*time.Millisecond)
		}
		ts += OpusFrameMs * time.Millisecond
	}
}

func TestOpusEncoderSetBitrate(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 2, BitrateBps: 32_000, CBR: true})
	tone := genTone(0, enc.FrameSamples(), 2, 440, 0.5)

	low, err := enc.Encode(tone, 0)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := enc.SetBitrate(256_000); err != nil {
		t.Fatalf("SetBitrate: %v", err)
	}
	// 重建编码器会丢一帧跨帧状态，多编几帧让码率控制器稳定下来。
	var high *EncodedFrame
	for i := 0; i < 5; i++ {
		high, err = enc.Encode(tone, 0)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	if len(high.Data) <= len(low.Data) {
		t.Errorf("after SetBitrate(256000) packet is %d bytes, not larger than the 32k one (%d bytes)",
			len(high.Data), len(low.Data))
	}
}

func TestOpusEncoderSetBitrateRejectsZero(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{})
	if err := enc.SetBitrate(0); err == nil {
		t.Fatal("SetBitrate(0) succeeded, want error")
	}
}

func TestOpusEncoderFlushAndRelease(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{})
	frames, err := enc.Flush()
	if err != nil || frames != nil {
		t.Errorf("Flush()=(%v, %v), want (nil, nil): Opus leaves nothing buffered", frames, err)
	}
	if err := enc.SetBitrate(48_000); err != nil {
		t.Fatalf("SetBitrate: %v", err)
	}
	enc.Release()
	if _, err := enc.Encode(make([]float32, enc.FrameSamples()*2), 0); err == nil {
		t.Error("Encode after Release succeeded, want error")
	}
	if err := enc.SetBitrate(48_000); err == nil {
		t.Error("SetBitrate after Release succeeded, want error")
	}
	if got := enc.PreSkip(); got != 0 {
		t.Errorf("PreSkip() after Release = %d, want 0", got)
	}
}

func TestF32ToS16(t *testing.T) {
	cases := []struct {
		name string
		in   float32
		want int16
	}{
		{"zero", 0, 0},
		{"plus one", 1.0, 32767},
		{"minus one", -1.0, -32767}, // -1.0 * 32767 = -32767，不到 -32768
		{"over range clamps", 2.0, 32767},
		{"under range clamps", -2.0, -32768},
		{"positive infinity", float32(math.Inf(1)), 32767},
		{"negative infinity", float32(math.Inf(-1)), -32768},
		{"NaN is silence", float32(math.NaN()), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst := make([]int16, 1)
			f32ToS16(dst, []float32{c.in})
			if dst[0] != c.want {
				t.Errorf("f32ToS16(%v)=%d, want %d", c.in, dst[0], c.want)
			}
		})
	}
}

// TestS16ToF32RoundTrip 守住编码/解码两侧缩放的对称性：int16 的满幅不能
// 在往返后超出 [-1, 1]，否则播放器会削波。
func TestS16ToF32RoundTrip(t *testing.T) {
	for _, v := range []int16{-32768, -32767, -1, 0, 1, 32766, 32767} {
		f32 := make([]float32, 1)
		s16ToF32(f32, []int16{v})
		if f32[0] > 1.0 || f32[0] < -1.0 {
			t.Errorf("s16ToF32(%d)=%v, want within [-1, 1]", v, f32[0])
		}
		back := make([]int16, 1)
		f32ToS16(back, f32)
		if diff := back[0] - v; diff > 1 || diff < -1 {
			t.Errorf("round trip %d -> %v -> %d", v, f32[0], back[0])
		}
	}
}

func BenchmarkOpusEncodeStereo(b *testing.B) {
	enc, err := NewOpusEncoder(OpusEncoderConfig{Channels: 2, BitrateBps: 128_000, Complexity: 5})
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Release()
	pcm := genTone(0, enc.FrameSamples(), 2, 440, 0.5)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := enc.Encode(pcm, 0); err != nil {
			b.Fatal(err)
		}
	}
}
