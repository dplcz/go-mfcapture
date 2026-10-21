package mfcapture

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"mfcapture/audio"
)

// decodeTestTone 生成 freq Hz 的交错正弦。startSample 是这段信号在整条流里的
// 起始位置，保证跨帧相位连续——每帧从 0 相位重启会在帧边界产生宽带咔哒声，
// 拉低编码质量，让往返测试测到素材而不是代码。
func decodeTestTone(startSample, samples, channels int, freq float64, amp float32) []float32 {
	pcm := make([]float32, samples*channels)
	for i := 0; i < samples; i++ {
		v := float32(math.Sin(2*math.Pi*freq*float64(startSample+i)/float64(audio.OpusSampleRate))) * amp
		for c := 0; c < channels; c++ {
			pcm[i*channels+c] = v
		}
	}
	return pcm
}

func newTestAudioEncoder(t *testing.T, channels int) *audio.OpusEncoder {
	t.Helper()
	enc, err := audio.NewOpusEncoder(audio.OpusEncoderConfig{
		Channels:   channels,
		BitrateBps: 128_000,
	})
	if err != nil {
		t.Fatalf("NewOpusEncoder: %v", err)
	}
	t.Cleanup(enc.Release)
	return enc
}

// encodeTestPackets 按 20 ms 切帧并编码，返回复制过的包——Encode 的 Data 指向
// 编码器内部缓冲，下一次编码就会覆盖。
func encodeTestPackets(t *testing.T, enc *audio.OpusEncoder, stream []float32) [][]byte {
	t.Helper()
	samples := enc.FrameSamples() * enc.Channels()
	var packets [][]byte
	for off := 0; off+samples <= len(stream); off += samples {
		f, err := enc.Encode(stream[off:off+samples], 0)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		packets = append(packets, append([]byte(nil), f.Data...))
	}
	return packets
}

// pcmRecord 是 pcmSink 保存的一帧快照。PCM 按零拷贝契约只在回调内有效，
// 所以这里必须复制后才能跨回调断言。
type pcmRecord struct {
	pcm []float32
	ts  time.Duration
	dur time.Duration
	plc bool
}

type pcmSink struct {
	mu     sync.Mutex
	frames []pcmRecord
	events []AudioEvent
}

func (s *pcmSink) OnPCMFrame(f *audio.DecodedFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, pcmRecord{
		pcm: append([]float32(nil), f.PCM...),
		ts:  f.Timestamp,
		dur: f.Duration,
		plc: f.PLC,
	})
}

func (s *pcmSink) OnAudioEvent(e AudioEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *pcmSink) snapshot() ([]pcmRecord, []AudioEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pcmRecord(nil), s.frames...), append([]AudioEvent(nil), s.events...)
}

// decodeSessionRun 管理一个跑在 goroutine 上的 Run，保证测试结束时一定会
// cancel 并等它返回。
type decodeSessionRun struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

func startDecodeSession(t *testing.T, s *AudioDecodeSession) *decodeSessionRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &decodeSessionRun{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- s.Run(ctx) }()
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *decodeSessionRun) stop(t *testing.T) error {
	t.Helper()
	r.once.Do(func() {
		r.cancel()
		select {
		case r.err = <-r.done:
		case <-time.After(2 * time.Second):
			t.Error("AudioDecodeSession.Run did not return within 2s of cancel")
		}
	})
	return r.err
}

func waitForPCMFrames(t *testing.T, sink *pcmSink, want int) []pcmRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		frames, _ := sink.snapshot()
		if len(frames) >= want {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d PCM frames, got %d", want, len(frames))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAudioDecodeSessionRoundTrip 覆盖 P5 的主路径：编码→Submit→Run→解码→
// OnPCMFrame。断言帧长、PTS 连续、pre-skip 裁剪和内容保真度。
func TestAudioDecodeSessionRoundTrip(t *testing.T) {
	const (
		channels = 2
		frames   = 50
	)
	frameDur := time.Duration(audio.OpusFrameMs) * time.Millisecond

	enc := newTestAudioEncoder(t, channels)
	stream := decodeTestTone(0, frames*audio.OpusFrameSamples, channels, 440, 0.5)
	packets := encodeTestPackets(t, enc, stream)
	preSkip := enc.PreSkip()

	sink := &pcmSink{}
	s, err := NewAudioDecodeSession(AudioDecodeSessionConfig{
		Channels: channels,
		PreSkip:  preSkip,
	}, sink)
	if err != nil {
		t.Fatalf("NewAudioDecodeSession: %v", err)
	}
	run := startDecodeSession(t, s)
	for i, pkt := range packets {
		if !s.Submit(pkt, time.Duration(i)*frameDur) {
			t.Fatalf("Submit(%d) = false", i)
		}
	}

	got := waitForPCMFrames(t, sink, frames)
	if err := run.stop(t); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	_ = s.Close()

	_, events := sink.snapshot()
	if len(events) == 0 || events[0].Kind != AudioDescReady {
		t.Fatalf("events = %+v, want AudioDescReady first", events)
	}
	d := events[0].Desc
	if d.Codec != "opus" || d.SampleRate != audio.OpusSampleRate ||
		d.Channels != channels || d.FrameMs != audio.OpusFrameMs || d.PreSkip != preSkip {
		t.Errorf("desc = %+v, want opus / 48000 Hz / %d ch / 20 ms / pre-skip %d",
			d, channels, preSkip)
	}

	if len(got) != frames {
		t.Fatalf("got %d PCM frames, want %d", len(got), frames)
	}
	// 第一帧被 pre-skip 裁短，之后每帧都是满的 20 ms。
	if n := len(got[0].pcm); n != (audio.OpusFrameSamples-preSkip)*channels {
		t.Errorf("first frame has %d samples, want %d", n, (audio.OpusFrameSamples-preSkip)*channels)
	}
	if want := time.Duration(preSkip) * time.Second / audio.OpusSampleRate; got[0].ts != want {
		t.Errorf("first PTS = %v, want %v (shifted by pre-skip)", got[0].ts, want)
	}
	for i := 1; i < len(got); i++ {
		if n := len(got[i].pcm); n != audio.OpusFrameSamples*channels {
			t.Errorf("frame %d has %d samples, want %d", i, n, audio.OpusFrameSamples*channels)
		}
		if want := got[i-1].ts + got[i-1].dur; got[i].ts != want {
			t.Errorf("frame %d PTS = %v, want %v (contiguous)", i, got[i].ts, want)
		}
		if got[i].plc {
			t.Errorf("frame %d marked PLC, want real audio", i)
		}
	}
	last := got[len(got)-1]
	if want := time.Duration(frames) * frameDur; last.ts+last.dur != want {
		t.Errorf("timeline ends at %v, want %v", last.ts+last.dur, want)
	}

	// 拼起来和原始信号做相关：内容对不上说明链路接线错了。
	var decoded []float32
	for _, f := range got {
		decoded = append(decoded, f.pcm...)
	}
	best, lag := bestAudioCorrelation(stream, decoded, audio.OpusFrameSamples*channels)
	if best < 0.95 {
		t.Errorf("best correlation %.4f at lag %d, want >= 0.95", best, lag)
	}
	if lag > audio.OpusFrameSamples*channels {
		t.Errorf("best lag %d exceeds one frame: decoded stream is misaligned", lag)
	}
	t.Logf("correlation %.4f at lag %d (pre-skip %d)", best, lag, preSkip)
}

// TestAudioDecodeSessionPLC 验证丢包时 SubmitPLC 生成 20 ms 补偿帧，且时间线
// 不留空洞。
func TestAudioDecodeSessionPLC(t *testing.T) {
	const (
		channels = 2
		frames   = 10
		lost     = 5
	)
	frameDur := time.Duration(audio.OpusFrameMs) * time.Millisecond

	enc := newTestAudioEncoder(t, channels)
	stream := decodeTestTone(0, frames*audio.OpusFrameSamples, channels, 440, 0.5)
	packets := encodeTestPackets(t, enc, stream)

	sink := &pcmSink{}
	s, err := NewAudioDecodeSession(AudioDecodeSessionConfig{
		Channels: channels,
		PreSkip:  enc.PreSkip(),
	}, sink)
	if err != nil {
		t.Fatalf("NewAudioDecodeSession: %v", err)
	}
	run := startDecodeSession(t, s)

	for i := 0; i < lost; i++ {
		if !s.Submit(packets[i], time.Duration(i)*frameDur) {
			t.Fatalf("Submit(%d) = false", i)
		}
	}
	if !s.SubmitPLC(time.Duration(lost) * frameDur) {
		t.Fatal("SubmitPLC = false")
	}
	// 第 lost 个包就是"丢失"的那个，从下一个开始继续投递。
	for i := lost + 1; i < frames; i++ {
		if !s.Submit(packets[i], time.Duration(i)*frameDur) {
			t.Fatalf("Submit(%d) = false", i)
		}
	}

	// 10 个包丢 1 个：9 个真实帧 + 1 个补偿帧。
	got := waitForPCMFrames(t, sink, frames)
	run.stop(t)

	plc := got[lost]
	if !plc.plc {
		t.Error("concealed frame is not marked PLC")
	}
	if n := len(plc.pcm); n != audio.OpusFrameSamples*channels {
		t.Errorf("PLC frame has %d samples, want %d", n, audio.OpusFrameSamples*channels)
	}
	if plc.dur != frameDur {
		t.Errorf("PLC frame Duration = %v, want %v", plc.dur, frameDur)
	}
	if want := time.Duration(lost) * frameDur; plc.ts != want {
		t.Errorf("PLC frame PTS = %v, want %v", plc.ts, want)
	}
	if want := got[lost-1].ts + got[lost-1].dur; plc.ts != want {
		t.Errorf("PLC frame starts at %v, want %v (no hole)", plc.ts, want)
	}
	if next := got[lost+1]; next.plc {
		t.Error("frame after the lost packet is marked PLC")
	} else if want := plc.ts + plc.dur; next.ts != want {
		t.Errorf("frame after PLC starts at %v, want %v", next.ts, want)
	}

	// 补偿帧应该延续前文的音调，而不是一段全零。
	nonzero := false
	for _, v := range plc.pcm {
		if v != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		t.Error("PLC frame is all zeros")
	}
}

// TestAudioDecodeSessionDecodeError 验证坏包只丢一帧并上报事件，会话继续运行。
func TestAudioDecodeSessionDecodeError(t *testing.T) {
	frameDur := time.Duration(audio.OpusFrameMs) * time.Millisecond

	enc := newTestAudioEncoder(t, 1)
	stream := decodeTestTone(0, audio.OpusFrameSamples, 1, 440, 0.5)
	packets := encodeTestPackets(t, enc, stream)

	sink := &pcmSink{}
	s, err := NewAudioDecodeSession(AudioDecodeSessionConfig{
		Channels: 1,
		PreSkip:  enc.PreSkip(),
	}, sink)
	if err != nil {
		t.Fatalf("NewAudioDecodeSession: %v", err)
	}
	run := startDecodeSession(t, s)

	if !s.Submit([]byte{0xFF}, 0) {
		t.Fatal("Submit(invalid) = false")
	}
	if !s.Submit(packets[0], frameDur) {
		t.Fatal("Submit(valid) = false")
	}

	got := waitForPCMFrames(t, sink, 1)
	run.stop(t)

	if len(got) != 1 {
		t.Fatalf("got %d PCM frames, want 1 (bad packet skipped, good one decoded)", len(got))
	}
	_, events := sink.snapshot()
	var decodeErrs int
	for _, e := range events {
		if e.Kind != AudioDecodeError {
			continue
		}
		decodeErrs++
		if e.Err == nil {
			t.Error("AudioDecodeError event carries no error")
		}
	}
	if decodeErrs != 1 {
		t.Errorf("got %d AudioDecodeError events, want 1", decodeErrs)
	}
}

// TestAudioDecodeSessionPreSkipDisabled 验证负的 PreSkip 原样传给解码器：
// 发送端已经扣过时延，接收端不再裁。
func TestAudioDecodeSessionPreSkipDisabled(t *testing.T) {
	frameDur := time.Duration(audio.OpusFrameMs) * time.Millisecond

	enc := newTestAudioEncoder(t, 1)
	stream := decodeTestTone(0, 3*audio.OpusFrameSamples, 1, 440, 0.5)
	packets := encodeTestPackets(t, enc, stream)

	sink := &pcmSink{}
	s, err := NewAudioDecodeSession(AudioDecodeSessionConfig{Channels: 1, PreSkip: -1}, sink)
	if err != nil {
		t.Fatalf("NewAudioDecodeSession: %v", err)
	}
	run := startDecodeSession(t, s)
	for i, pkt := range packets {
		if !s.Submit(pkt, time.Duration(i)*frameDur) {
			t.Fatalf("Submit(%d) = false", i)
		}
	}

	got := waitForPCMFrames(t, sink, len(packets))
	run.stop(t)

	if n := len(got[0].pcm); n != audio.OpusFrameSamples {
		t.Errorf("first frame has %d samples, want %d (pre-skip trimming disabled)",
			n, audio.OpusFrameSamples)
	}
	if got[0].ts != 0 {
		t.Errorf("first PTS = %v, want 0", got[0].ts)
	}
	_, events := sink.snapshot()
	if len(events) == 0 || events[0].Desc.PreSkip != 0 {
		t.Errorf("desc PreSkip = %+v, want 0 (receiver-side trim amount)", events)
	}
}

// TestAudioDecodeSessionSubmitCopiesData 验证 Submit 复制了输入：网络层在投递
// 后立即复用接收缓冲不会破坏待解码的数据。
func TestAudioDecodeSessionSubmitCopiesData(t *testing.T) {
	enc := newTestAudioEncoder(t, 1)
	stream := decodeTestTone(0, audio.OpusFrameSamples, 1, 440, 0.5)
	packets := encodeTestPackets(t, enc, stream)

	sink := &pcmSink{}
	s, err := NewAudioDecodeSession(AudioDecodeSessionConfig{
		Channels:   1,
		PreSkip:    enc.PreSkip(),
		BufferSize: 4,
	}, sink)
	if err != nil {
		t.Fatalf("NewAudioDecodeSession: %v", err)
	}

	pkt := append([]byte(nil), packets[0]...)
	if !s.Submit(pkt, 0) {
		t.Fatal("Submit = false")
	}
	for i := range pkt {
		pkt[i] = 0 // 模拟传输层复用缓冲
	}

	run := startDecodeSession(t, s)
	got := waitForPCMFrames(t, sink, 1)
	run.stop(t)

	var sum float64
	for _, v := range got[0].pcm {
		sum += float64(v) * float64(v)
	}
	rms := math.Sqrt(sum / float64(len(got[0].pcm)))
	if rms < 0.1 {
		t.Errorf("decoded RMS = %.3f, want ~0.35 (Submit did not copy the packet)", rms)
	}
}

func TestNewAudioDecodeSessionRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  AudioDecodeSessionConfig
	}{
		{"channels", AudioDecodeSessionConfig{Channels: 3}},
		{"buffer size", AudioDecodeSessionConfig{BufferSize: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewAudioDecodeSession(c.cfg, nil); err == nil {
				t.Fatalf("NewAudioDecodeSession(%+v) succeeded, want error", c.cfg)
			}
		})
	}
}

// TestAudioDecodeSessionSubmitQueueAndClose 覆盖队列满与关闭后的返回值语义。
func TestAudioDecodeSessionSubmitQueueAndClose(t *testing.T) {
	s, err := NewAudioDecodeSession(AudioDecodeSessionConfig{Channels: 1, BufferSize: 1}, nil)
	if err != nil {
		t.Fatalf("NewAudioDecodeSession: %v", err)
	}

	pkt := []byte{0x01, 0x02}
	if !s.Submit(pkt, 0) {
		t.Error("Submit on an empty queue = false")
	}
	if s.Submit(pkt, 0) {
		t.Error("Submit accepted a packet into a full queue")
	}
	if s.Submit(nil, 0) {
		t.Error("Submit accepted an empty packet")
	}
	if s.SubmitPLC(0) {
		t.Error("SubmitPLC accepted a packet into a full queue")
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if s.Submit(pkt, 0) {
		t.Error("Submit after Close = true")
	}
	if s.SubmitPLC(0) {
		t.Error("SubmitPLC after Close = true")
	}
}

// bestAudioCorrelation 在 ref 上滑动 got，返回最高归一化相关系数与对应 lag。
func bestAudioCorrelation(ref, got []float32, maxLag int) (float64, int) {
	n := len(got)
	if n == 0 || n > len(ref) {
		return 0, 0
	}
	limit := len(ref) - n
	if limit > maxLag {
		limit = maxLag
	}
	best, bestLag := -1.0, 0
	for lag := 0; lag <= limit; lag++ {
		if c := audioCorrelation(ref[lag:lag+n], got); c > best {
			best, bestLag = c, lag
		}
	}
	return best, bestLag
}

func audioCorrelation(a, b []float32) float64 {
	var sa, sb, sab float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		sa += x * x
		sb += y * y
		sab += x * y
	}
	if sa == 0 || sb == 0 {
		return 0
	}
	return sab / (math.Sqrt(sa) * math.Sqrt(sb))
}
