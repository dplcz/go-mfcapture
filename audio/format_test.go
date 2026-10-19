package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func testFormat(float32Samples bool) Format {
	return Format{SampleRate: OpusSampleRate, Channels: 2, Float32: float32Samples}
}

func TestFormatValidate(t *testing.T) {
	good := []Format{
		{SampleRate: OpusSampleRate, Channels: 1, Float32: true},
		{SampleRate: OpusSampleRate, Channels: 2},
	}
	for _, f := range good {
		if err := f.Validate(); err != nil {
			t.Errorf("Validate(%+v)=%v, want nil", f, err)
		}
	}

	bad := []struct {
		name string
		f    Format
	}{
		{"zero rate", Format{Channels: 2}},
		{"negative rate", Format{SampleRate: -48000, Channels: 2}},
		{"no channels", Format{SampleRate: 48000}},
		{"three channels", Format{SampleRate: 48000, Channels: 3}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := c.f.Validate(); err == nil {
				t.Errorf("Validate(%+v)=nil, want error", c.f)
			}
		})
	}
}

func TestFormatLayout(t *testing.T) {
	f32 := Format{SampleRate: 48000, Channels: 2, Float32: true}
	if got := f32.BytesPerSample(); got != 4 {
		t.Errorf("BytesPerSample()=%d, want 4", got)
	}
	if got := f32.BlockAlign(); got != 8 {
		t.Errorf("BlockAlign()=%d, want 8", got)
	}
	if got := f32.Samples(make([]byte, 1920)); got != 240 {
		t.Errorf("Samples(1920 bytes)=%d, want 240", got)
	}
	if got := f32.FrameBytes(960); got != 7680 {
		t.Errorf("FrameBytes(960)=%d, want 7680", got)
	}

	s16 := Format{SampleRate: 48000, Channels: 1}
	if got := s16.BytesPerSample(); got != 2 {
		t.Errorf("S16 BytesPerSample()=%d, want 2", got)
	}
	if got := s16.BlockAlign(); got != 2 {
		t.Errorf("S16 BlockAlign()=%d, want 2", got)
	}
	if got := s16.Samples(make([]byte, 1920)); got != 960 {
		t.Errorf("S16 Samples(1920 bytes)=%d, want 960", got)
	}
}

func TestFormatToFloat32Float(t *testing.T) {
	f := Format{SampleRate: 48000, Channels: 2, Float32: true}
	// 长度必须是 BlockAlign（2 声道 x 4 字节）的整数倍，所以取偶数个采样。
	vals := []float32{0, 1, -1, 0.5, -0.25, -0.5, float32(math.Inf(1)), float32(math.NaN())}

	data := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(v))
	}

	dst := make([]float32, len(vals))
	n, err := f.ToFloat32(dst, data)
	if err != nil {
		t.Fatalf("ToFloat32: %v", err)
	}
	if n != len(vals) {
		t.Fatalf("ToFloat32 wrote %d values, want %d", n, len(vals))
	}
	for i := range vals {
		// 按位比较：NaN 不等于自身，但位模式必须原样保留。
		if got, want := math.Float32bits(dst[i]), math.Float32bits(vals[i]); got != want {
			t.Errorf("sample %d: bits %#x, want %#x", i, got, want)
		}
	}
}

func TestFormatToFloat32S16(t *testing.T) {
	f := Format{SampleRate: 48000, Channels: 1}
	vals := []int16{-32768, -16384, 0, 16384, 32767}

	data := make([]byte, len(vals)*2)
	for i, v := range vals {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(v))
	}

	dst := make([]float32, len(vals))
	n, err := f.ToFloat32(dst, data)
	if err != nil {
		t.Fatalf("ToFloat32: %v", err)
	}
	if n != len(vals) {
		t.Fatalf("ToFloat32 wrote %d values, want %d", n, len(vals))
	}
	want := []float32{-1, -0.5, 0, 0.5, 32767.0 / 32768.0}
	for i := range want {
		if dst[i] != want[i] {
			t.Errorf("sample %d: %v, want %v", i, dst[i], want[i])
		}
	}
}

func TestFormatToFloat32Errors(t *testing.T) {
	f := Format{SampleRate: 48000, Channels: 2, Float32: true}

	if _, err := f.ToFloat32(make([]float32, 8), make([]byte, 7)); err == nil {
		t.Error("ToFloat32 accepted a 7-byte buffer, want a block-align error")
	}
	if _, err := f.ToFloat32(make([]float32, 1), make([]byte, 8)); err == nil {
		t.Error("ToFloat32 accepted a too-small destination")
	}
	if _, err := (Format{}).ToFloat32(make([]float32, 2), make([]byte, 8)); err == nil {
		t.Error("ToFloat32 accepted an invalid Format")
	}
}
