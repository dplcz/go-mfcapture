package audio

import (
	"errors"
	"testing"
	"time"
)

func mustFramer(t *testing.T, maxLatency time.Duration) *Framer {
	t.Helper()
	f, err := NewFramer(FramerConfig{Format: testFormat(false), MaxLatency: maxLatency})
	if err != nil {
		t.Fatalf("NewFramer: %v", err)
	}
	return f
}

// capturedFrame 是 Framer 输出帧的快照。Framer 复用内部缓冲，测试要跨调用
// 保留数据就必须复制。
type capturedFrame struct {
	data     []byte
	ts       time.Duration
	duration time.Duration
	silent   bool
}

func snapshot(f *Frame) capturedFrame {
	return capturedFrame{
		data:     append([]byte(nil), f.Data...),
		ts:       f.Timestamp,
		duration: f.Duration,
		silent:   f.Silent,
	}
}

// pump 模拟 AudioPipeline 的主循环：读一帧采集数据、喂给 Framer、排空可用
// 的 20 ms 帧；采集源超时且没有积压时补一帧静音保持 50 pps。
func pump(t *testing.T, cap Capturer, fr *Framer, steps int, timeout time.Duration, silence bool) []capturedFrame {
	t.Helper()
	var out []capturedFrame
	for i := 0; i < steps; i++ {
		frame, err := cap.ReadFrame(timeout)
		timedOut := errors.Is(err, ErrTimeout)
		switch {
		case err == nil:
			if err := fr.Append(frame); err != nil {
				t.Fatalf("step %d: Append: %v", i, err)
			}
		case timedOut:
			// loopback 在系统静音时就是这种状态，不是错误。
		default:
			t.Fatalf("step %d: ReadFrame: %v", i, err)
		}

		for {
			f, ok := fr.Take()
			if !ok {
				break
			}
			out = append(out, snapshot(f))
		}
		if timedOut && silence && fr.Available() == 0 {
			out = append(out, snapshot(fr.Silence()))
		}
	}
	return out
}

func TestNewFramerRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  FramerConfig
	}{
		{"invalid format", FramerConfig{Format: Format{SampleRate: 48000, Channels: 3}}},
		{"needs resampling", FramerConfig{Format: Format{SampleRate: 44100, Channels: 2}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewFramer(c.cfg); err == nil {
				t.Fatalf("NewFramer(%+v) succeeded, want error", c.cfg)
			}
		})
	}
}

func TestFramerTenMsPackets(t *testing.T) {
	const packets = 20 // 20 x 10 ms = 200 ms
	chunks := make([]fakeChunk, packets)
	for i := range chunks {
		chunks[i] = fakeChunk{
			at:   time.Duration(i) * 10 * time.Millisecond,
			data: rampS16(480, 2, i*480),
		}
	}
	cap := newFakeCapturer(testFormat(false), chunks...)
	fr := mustFramer(t, 0)

	out := pump(t, cap, fr, packets+5, 20*time.Millisecond, false)
	if len(out) != 10 {
		t.Fatalf("emitted %d frames, want 10 (200 ms / 20 ms)", len(out))
	}
	for i, f := range out {
		want := time.Duration(i) * 20 * time.Millisecond
		if f.ts != want {
			t.Errorf("frame %d: PTS=%v, want %v", i, f.ts, want)
		}
		if f.duration != 20*time.Millisecond {
			t.Errorf("frame %d: Duration=%v, want 20ms", i, f.duration)
		}
		if f.silent {
			t.Errorf("frame %d unexpectedly marked silent", i)
		}
		if got := firstChannel(f.data, 2)[0]; got != int16(i*960) {
			t.Errorf("frame %d: first sample=%d, want %d", i, got, i*960)
		}
	}
	if got := fr.DroppedFrames(); got != 0 {
		t.Errorf("DroppedFrames()=%d, want 0", got)
	}
}

// TestFramerThirtyMsPackets 覆盖"包长不是帧长整数倍"的节奏：30 ms 的包会
// 留下 10 ms 尾巴，下一次 Append 补满后一次产出两帧。长期仍应是 50 pps。
func TestFramerThirtyMsPackets(t *testing.T) {
	const packets = 10 // 10 x 30 ms = 300 ms
	chunks := make([]fakeChunk, packets)
	for i := range chunks {
		chunks[i] = fakeChunk{
			at:   time.Duration(i) * 30 * time.Millisecond,
			data: rampS16(1440, 2, i*1440),
		}
	}
	cap := newFakeCapturer(testFormat(false), chunks...)
	fr := mustFramer(t, 0)

	out := pump(t, cap, fr, packets+10, 20*time.Millisecond, false)
	if len(out) != 15 {
		t.Fatalf("emitted %d frames, want 15 (300 ms / 20 ms)", len(out))
	}
	for i, f := range out {
		want := time.Duration(i) * 20 * time.Millisecond
		if f.ts != want {
			t.Errorf("frame %d: PTS=%v, want %v", i, f.ts, want)
		}
		if f.silent {
			t.Errorf("frame %d unexpectedly marked silent", i)
		}
	}
	if got := fr.DroppedFrames(); got != 0 {
		t.Errorf("DroppedFrames()=%d, want 0", got)
	}
}

// TestFramerSilenceFill 模拟 loopback 在系统静音时不产包：超时期间必须按
// 20 ms 推进 PTS，恢复后的第一帧要接着静音的时间线，不能出现空洞或倒退。
func TestFramerSilenceFill(t *testing.T) {
	cap := newFakeCapturer(testFormat(false),
		fakeChunk{at: 0, data: rampS16(960, 2, 0)},
		fakeChunk{at: 200 * time.Millisecond, data: rampS16(960, 2, 960)},
	)
	fr := mustFramer(t, 0)

	out := pump(t, cap, fr, 11, 20*time.Millisecond, true)
	if len(out) != 11 {
		t.Fatalf("emitted %d frames, want 11 (1 real + 9 silent + 1 real)", len(out))
	}
	for i, f := range out {
		want := time.Duration(i) * 20 * time.Millisecond
		if f.ts != want {
			t.Errorf("frame %d: PTS=%v, want %v", i, f.ts, want)
		}
		wantSilent := i > 0 && i < 10
		if f.silent != wantSilent {
			t.Errorf("frame %d: Silent=%v, want %v", i, f.silent, wantSilent)
		}
		if f.silent {
			for _, b := range f.data {
				if b != 0 {
					t.Fatalf("silent frame %d contains non-zero data", i)
				}
			}
		}
	}
}

func TestFramerBacklogDropsOldest(t *testing.T) {
	// 一个 400 ms 的巨型包；MaxLatency=100 ms 只留最新的 5 帧。
	cap := newFakeCapturer(testFormat(false),
		fakeChunk{at: 0, data: rampS16(19200, 2, 0)},
	)
	fr := mustFramer(t, 100*time.Millisecond)

	out := pump(t, cap, fr, 1, 20*time.Millisecond, false)
	if len(out) != 5 {
		t.Fatalf("emitted %d frames, want 5 (100 ms latency)", len(out))
	}
	for i, f := range out {
		want := time.Duration(300+i*20) * time.Millisecond
		if f.ts != want {
			t.Errorf("frame %d: PTS=%v, want %v", i, f.ts, want)
		}
	}
	if got := fr.DroppedFrames(); got != 15 {
		t.Errorf("DroppedFrames()=%d, want 15", got)
	}
	if got := firstChannel(out[0].data, 2)[0]; got != 14400 {
		t.Errorf("first kept sample=%d, want 14400 (300 ms dropped)", got)
	}
}

// TestFramerResumeClampsPTS 验证补过静音后源时间戳略早于下一帧时保持单调，
// 而不是让接收端的抖动缓冲收到倒退的 PTS。
func TestFramerResumeClampsPTS(t *testing.T) {
	fr := mustFramer(t, 0)
	if err := fr.Append(&Frame{Data: rampS16(960, 2, 0), Timestamp: 0}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	first, ok := fr.Take()
	if !ok || first.Timestamp != 0 {
		t.Fatalf("first frame = (%v, %v), want PTS 0", first, ok)
	}
	if got := fr.Silence().Timestamp; got != 20*time.Millisecond {
		t.Fatalf("silence PTS=%v, want 20ms", got)
	}

	// 源时间戳比静音推进后的时间线早 30 ms：应钳到 40 ms。
	if err := fr.Append(&Frame{Data: rampS16(960, 2, 0), Timestamp: 10 * time.Millisecond}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	next, ok := fr.Take()
	if !ok {
		t.Fatal("Take after resume returned no frame")
	}
	if got, want := next.Timestamp, 40*time.Millisecond; got != want {
		t.Errorf("resumed PTS=%v, want %v (clamped to the timeline)", got, want)
	}
}

func TestFramerEmptyAndMisalignedAppend(t *testing.T) {
	fr := mustFramer(t, 0)
	if err := fr.Append(nil); err != nil {
		t.Errorf("Append(nil)=%v, want nil", err)
	}
	if err := fr.Append(&Frame{Timestamp: 5 * time.Second}); err != nil {
		t.Errorf("Append(empty)=%v, want nil", err)
	}
	if got := fr.NextPTS(); got != 0 {
		t.Errorf("empty append moved NextPTS to %v", got)
	}

	if err := fr.Append(&Frame{Data: make([]byte, 3)}); err == nil {
		t.Error("Append accepted a misaligned buffer")
	}
	if got := fr.Available(); got != 0 {
		t.Errorf("Available()=%d after rejected append, want 0", got)
	}
}

func TestFramerReset(t *testing.T) {
	fr := mustFramer(t, 0)
	if err := fr.Append(&Frame{Data: rampS16(960, 2, 0), Timestamp: 5 * time.Second}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if f, ok := fr.Take(); !ok || f.Timestamp != 5*time.Second {
		t.Fatalf("frame before Reset: (%v, %v)", f, ok)
	}

	fr.Reset()
	if got := fr.NextPTS(); got != 0 {
		t.Errorf("NextPTS()=%v after Reset, want 0", got)
	}
	if err := fr.Append(&Frame{Data: rampS16(960, 2, 0), Timestamp: 0}); err != nil {
		t.Fatalf("Append after Reset: %v", err)
	}
	if f, ok := fr.Take(); !ok || f.Timestamp != 0 {
		t.Errorf("frame after Reset: (%v, %v), want PTS 0", f, ok)
	}
}

func TestFramerReleaseCapturer(t *testing.T) {
	cap := newFakeCapturer(testFormat(false))
	cap.Release()
	if _, err := cap.ReadFrame(10 * time.Millisecond); err == nil {
		t.Error("ReadFrame after Release succeeded, want error")
	}
}

func TestFramerAnchor(t *testing.T) {
	fr := mustFramer(t, 0)
	if fr.Started() {
		t.Error("Started()=true on a fresh Framer")
	}
	fr.Anchor(3 * time.Second)
	if !fr.Started() {
		t.Fatal("Started()=false after Anchor")
	}
	if got := fr.NextPTS(); got != 3*time.Second {
		t.Errorf("NextPTS()=%v after Anchor, want 3s", got)
	}

	// 已锚定后 Anchor 不生效。
	fr.Anchor(10 * time.Second)
	if got := fr.NextPTS(); got != 3*time.Second {
		t.Errorf("NextPTS()=%v after second Anchor, want 3s", got)
	}

	// Silence 从锚点开始推进，数据到来时保持单调。
	if s := fr.Silence(); s.Timestamp != 3*time.Second || !s.Silent {
		t.Errorf("Silence() = %+v, want PTS 3s and Silent", s)
	}
	if err := fr.Append(&Frame{Data: rampS16(960, 2, 0), Timestamp: 2 * time.Second}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if f, ok := fr.Take(); !ok || f.Timestamp != 3020*time.Millisecond {
		t.Errorf("frame after Anchor = (%v, %v), want PTS 3.02s", f, ok)
	}
}
