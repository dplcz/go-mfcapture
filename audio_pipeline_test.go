package mfcapture

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"mfcapture/audio"
)

// fakeAudioCapturer 是一个可编程的 Capturer：按顺序返回预设的包或错误，
// 不阻塞、不占用真实时间。时间戳由测试直接给定，模拟 WASAPI 已经锚定 epoch。
type fakeAudioCapturer struct {
	format audio.Format
	steps  []fakeAudioStep
	next   int
	closed bool
}

type fakeAudioStep struct {
	ts   time.Duration
	data []byte
	err  error
}

var _ audio.Capturer = (*fakeAudioCapturer)(nil)

func (c *fakeAudioCapturer) Format() audio.Format { return c.format }

func (c *fakeAudioCapturer) ReadFrame(timeout time.Duration) (*audio.Frame, error) {
	if c.closed {
		return nil, errors.New("fake: released")
	}
	if c.next >= len(c.steps) {
		// 脚本耗尽后一直超时，模拟静音的 loopback。
		time.Sleep(time.Millisecond)
		return nil, audio.ErrTimeout
	}
	step := c.steps[c.next]
	c.next++
	if step.err != nil {
		return nil, step.err
	}
	return &audio.Frame{
		Data:      step.data,
		Timestamp: step.ts,
		Duration:  time.Duration(len(step.data)/c.format.BlockAlign()) * time.Second / audio.OpusSampleRate,
	}, nil
}

func (c *fakeAudioCapturer) Release() { c.closed = true }

// pcmTone 生成一段 440 Hz 正弦，便于后期校验数据没被改写。
func pcmTone(sampleFrames, channels int, start int) []byte {
	f := audio.Format{SampleRate: audio.OpusSampleRate, Channels: channels, Float32: true}
	buf := make([]byte, f.FrameBytes(sampleFrames))
	for i := 0; i < sampleFrames; i++ {
		v := math.Sin(2 * math.Pi * 440 * float64(start+i) / audio.OpusSampleRate)
		bits := math.Float32bits(float32(v))
		for ch := 0; ch < channels; ch++ {
			binary.LittleEndian.PutUint32(buf[(i*channels+ch)*4:], bits)
		}
	}
	return buf
}

// collectSink 记录所有回调，供断言使用。
type collectSink struct {
	mu     sync.Mutex
	frames []*audio.EncodedFrame
	events []AudioEvent
}

func (s *collectSink) OnAudioFrame(f *audio.EncodedFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *f
	cp.Data = append([]byte(nil), f.Data...)
	s.frames = append(s.frames, &cp)
}

func (s *collectSink) OnAudioEvent(e AudioEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *collectSink) snapshot() ([]*audio.EncodedFrame, []AudioEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*audio.EncodedFrame(nil), s.frames...), append([]AudioEvent(nil), s.events...)
}

func testPipeline(t *testing.T, cfg AudioPipelineConfig, sink AudioSink, factory func(audio.WASAPICapturerConfig) (audio.Capturer, error)) *AudioPipeline {
	t.Helper()
	p, err := NewAudioPipeline(cfg, sink)
	if err != nil {
		t.Fatalf("NewAudioPipeline: %v", err)
	}
	if factory != nil {
		p.newCapturer = factory
	}
	return p
}

func runPipeline(t *testing.T, p *AudioPipeline, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return p.Run(ctx)
}

// TestAudioPipelineContinuous 用 10 ms 的包喂满 200 ms，验证分帧、播报描述与统计。
func TestAudioPipelineContinuous(t *testing.T) {
	const (
		channels = 2
		packetMs = 10
		packets  = 20
	)
	steps := make([]fakeAudioStep, 0, packets)
	for i := 0; i < packets; i++ {
		steps = append(steps, fakeAudioStep{
			ts:   time.Duration(i*packetMs) * time.Millisecond,
			data: pcmTone(audio.OpusSampleRate*packetMs/1000, channels, i*audio.OpusSampleRate*packetMs/1000),
		})
	}
	fake := &fakeAudioCapturer{
		format: audio.Format{SampleRate: audio.OpusSampleRate, Channels: channels, Float32: true},
		steps:  steps,
	}
	sink := &collectSink{}
	p := testPipeline(t, AudioPipelineConfig{
		Epoch:      time.Now(),
		Channels:   channels,
		BitrateBps: 96_000,
	}, sink, func(audio.WASAPICapturerConfig) (audio.Capturer, error) { return fake, nil })

	if err := runPipeline(t, p, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want context.DeadlineExceeded", err)
	}
	p.Close()

	frames, events := sink.snapshot()
	if len(events) == 0 || events[0].Kind != AudioDescReady {
		t.Fatalf("first event = %+v, want AudioDescReady", events[0])
	}
	desc := events[0].Desc
	if desc.Codec != "opus" || desc.SampleRate != audio.OpusSampleRate ||
		desc.Channels != channels || desc.FrameMs != audio.OpusFrameMs || desc.PreSkip <= 0 {
		t.Errorf("Desc = %+v", desc)
	}

	if len(frames) < packets/2 {
		t.Fatalf("delivered %d frames, want at least %d", len(frames), packets/2)
	}
	for i, f := range frames {
		if len(f.Data) == 0 {
			t.Errorf("frame %d has empty data", i)
		}
		if f.Duration != audio.OpusFrameMs*time.Millisecond {
			t.Errorf("frame %d duration = %v, want 20ms", i, f.Duration)
		}
		if i > 0 && f.Timestamp < frames[i-1].Timestamp {
			t.Errorf("frame %d PTS %v went backwards from %v", i, f.Timestamp, frames[i-1].Timestamp)
		}
	}

	stats := p.Stats()
	if stats.Frames != uint64(len(frames)) {
		t.Errorf("Stats.Frames = %d, want %d", stats.Frames, len(frames))
	}
	if stats.Bytes == 0 || stats.SilentFrames == 0 {
		t.Errorf("Stats = %+v, want non-zero bytes and some padded silence", stats)
	}
}

// TestAudioPipelinePadsSilence 在采集源完全没有数据时仍保持 50 pps 的时间线。
func TestAudioPipelinePadsSilence(t *testing.T) {
	fake := &fakeAudioCapturer{
		format: audio.Format{SampleRate: audio.OpusSampleRate, Channels: 2, Float32: true},
	}
	sink := &collectSink{}
	p := testPipeline(t, AudioPipelineConfig{Epoch: time.Now()}, sink,
		func(audio.WASAPICapturerConfig) (audio.Capturer, error) { return fake, nil })

	if err := runPipeline(t, p, 120*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want context.DeadlineExceeded", err)
	}
	p.Close()

	frames, _ := sink.snapshot()
	if len(frames) < 3 {
		t.Fatalf("delivered %d silence frames in 120ms, want at least 3", len(frames))
	}
	for i := 1; i < len(frames); i++ {
		if gap := frames[i].Timestamp - frames[i-1].Timestamp; gap != audio.OpusFrameMs*time.Millisecond {
			t.Errorf("frame %d PTS gap = %v, want 20ms", i, gap)
		}
	}
	stats := p.Stats()
	if stats.SilentFrames != stats.Frames {
		t.Errorf("SilentFrames=%d, Frames=%d; all frames should be padded silence",
			stats.SilentFrames, stats.Frames)
	}
}

// TestAudioPipelineNoSilence 关闭补静音后，没有数据就不编码。
func TestAudioPipelineNoSilence(t *testing.T) {
	fake := &fakeAudioCapturer{
		format: audio.Format{SampleRate: audio.OpusSampleRate, Channels: 2, Float32: true},
	}
	sink := &collectSink{}
	p := testPipeline(t, AudioPipelineConfig{Epoch: time.Now(), NoSilence: true}, sink,
		func(audio.WASAPICapturerConfig) (audio.Capturer, error) { return fake, nil })

	if err := runPipeline(t, p, 80*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want context.DeadlineExceeded", err)
	}
	p.Close()

	frames, _ := sink.snapshot()
	if len(frames) != 0 {
		t.Errorf("delivered %d frames with silence padding disabled, want 0", len(frames))
	}
}

// TestAudioPipelineReconnects 验证设备丢失后自动重建采集源并继续出帧。
func TestAudioPipelineReconnects(t *testing.T) {
	format := audio.Format{SampleRate: audio.OpusSampleRate, Channels: 2, Float32: true}
	first := &fakeAudioCapturer{format: format}
	for i := 0; i < 6; i++ {
		first.steps = append(first.steps, fakeAudioStep{
			ts:   time.Duration(i*10) * time.Millisecond,
			data: pcmTone(480, 2, i*480),
		})
	}
	first.steps = append(first.steps, fakeAudioStep{err: audio.ErrDeviceLost})

	second := &fakeAudioCapturer{format: format}
	for i := 0; i < 6; i++ {
		second.steps = append(second.steps, fakeAudioStep{
			ts:   500*time.Millisecond + time.Duration(i*10)*time.Millisecond,
			data: pcmTone(480, 2, i*480),
		})
	}

	var calls int
	factory := func(audio.WASAPICapturerConfig) (audio.Capturer, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}

	sink := &collectSink{}
	p := testPipeline(t, AudioPipelineConfig{Epoch: time.Now()}, sink, factory)
	if err := runPipeline(t, p, 150*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want context.DeadlineExceeded", err)
	}
	p.Close()

	if calls < 2 {
		t.Fatalf("capturer factory called %d times, want a reconnect", calls)
	}
	if !first.closed {
		t.Error("the lost capturer was not released")
	}

	frames, events := sink.snapshot()
	var failures, successes int
	for _, e := range events {
		if e.Kind != AudioDeviceChanged {
			continue
		}
		if e.Err != nil {
			failures++
		} else if e.Desc.SampleRate == audio.OpusSampleRate {
			successes++
		}
	}
	if failures == 0 || successes == 0 {
		t.Errorf("DeviceChanged events: %d failures, %d successes; want both", failures, successes)
	}
	if len(frames) < 6 {
		t.Errorf("delivered %d frames across the reconnect, want at least 6", len(frames))
	}

	// 重连后的包从 500 ms 开始：时间线允许向前跳，但不能倒退。
	for i := 1; i < len(frames); i++ {
		if frames[i].Timestamp < frames[i-1].Timestamp {
			t.Errorf("frame %d PTS %v went backwards from %v", i, frames[i].Timestamp, frames[i-1].Timestamp)
		}
	}
	if last := frames[len(frames)-1].Timestamp; last < 500*time.Millisecond {
		t.Errorf("last PTS = %v, want the post-reconnect timeline (>= 500ms)", last)
	}
}

// TestAudioPipelineSetBitrate 验证命令队列在 Run 线程生效并更新 Desc。
func TestAudioPipelineSetBitrate(t *testing.T) {
	fake := &fakeAudioCapturer{
		format: audio.Format{SampleRate: audio.OpusSampleRate, Channels: 2, Float32: true},
	}
	sink := &collectSink{}
	p := testPipeline(t, AudioPipelineConfig{Epoch: time.Now()}, sink,
		func(audio.WASAPICapturerConfig) (audio.Capturer, error) { return fake, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.Now().Add(200 * time.Millisecond)
	for len(p.Desc().Codec) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Desc().Codec == "" {
		t.Fatal("pipeline never reported a description")
	}
	p.SetBitrate(64_000)

	deadline = time.Now().Add(200 * time.Millisecond)
	for p.Desc().BitrateBps != 64_000 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.Desc().BitrateBps; got != 64_000 {
		t.Errorf("Desc.BitrateBps = %d, want 64000", got)
	}

	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run returned %v, want context.DeadlineExceeded", err)
	}
	p.Close()
}
