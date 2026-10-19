package audio

import (
	"math"
	"testing"
)

func mustDecoder(t *testing.T, cfg OpusDecoderConfig) *OpusDecoder {
	t.Helper()
	dec, err := NewOpusDecoder(cfg)
	if err != nil {
		t.Fatalf("NewOpusDecoder(%+v): %v", cfg, err)
	}
	t.Cleanup(dec.Release)
	return dec
}

// encodeFrames 把 stream 按 20 ms 切成帧并全部编码，返回按序的包。
// 调用方必须在使用完 packets 前把所有包复制出来——Encode 返回的 Data 指向
// 编码器的内部缓冲，下一次 Encode 就会覆盖它。
func encodeFrames(t *testing.T, enc *OpusEncoder, stream []float32) [][]byte {
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

func TestNewOpusDecoderRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  OpusDecoderConfig
	}{
		{"sample rate", OpusDecoderConfig{SampleRate: 44100}},
		{"channels", OpusDecoderConfig{Channels: 3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewOpusDecoder(c.cfg); err == nil {
				t.Fatalf("NewOpusDecoder(%+v) succeeded, want error", c.cfg)
			}
		})
	}
}

func TestNewOpusDecoderDefaults(t *testing.T) {
	dec := mustDecoder(t, OpusDecoderConfig{})
	if got := dec.SampleRate(); got != OpusSampleRate {
		t.Errorf("SampleRate()=%d, want %d", got, OpusSampleRate)
	}
	if got := dec.Channels(); got != 2 {
		t.Errorf("Channels()=%d, want 2", got)
	}
	if got := dec.FrameSamples(); got != 960 {
		t.Errorf("FrameSamples()=%d, want 960", got)
	}
}

// TestOpusDecoderPreSkip 是解码器相对裸 go-opus 多做的那一件事：库本身不裁
// pre-skip（那是容器的活），本项目没有容器，所以裁剪放在这里。
func TestOpusDecoderPreSkip(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 1, BitrateBps: 128_000})
	preSkip := enc.PreSkip()

	const frames = 10
	stream := genTone(0, enc.FrameSamples()*frames, 1, 440, 0.5)
	packets := encodeFrames(t, enc, stream)

	dec := mustDecoder(t, OpusDecoderConfig{Channels: 1, PreSkip: preSkip})
	var total int
	for i, pkt := range packets {
		f, err := dec.Decode(pkt, 0)
		if err != nil {
			t.Fatalf("Decode(%d): %v", i, err)
		}
		if f == nil {
			continue
		}
		total += len(f.PCM)
	}

	want := frames*enc.FrameSamples() - preSkip
	if total != want {
		t.Errorf("decoded %d samples, want %d (%d frames x %d - %d pre-skip)",
			total, want, frames, enc.FrameSamples(), preSkip)
	}

	// PreSkip 为负表示"发送端已经减过了"，解码器不应再裁。
	dec2 := mustDecoder(t, OpusDecoderConfig{Channels: 1, PreSkip: -1})
	var total2 int
	for _, pkt := range packets {
		f, err := dec2.Decode(pkt, 0)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		total2 += len(f.PCM)
	}
	if want2 := frames * enc.FrameSamples(); total2 != want2 {
		t.Errorf("with PreSkip disabled decoded %d samples, want %d", total2, want2)
	}
}

// TestOpusRoundTrip 端到端验证适配器接线：编码→解码后应还原出同一个信号，
// 且时延不超过一帧。用固定 440 Hz 正弦而不是噪声——Opus 压不动噪声
// （bench/opusbench 实测 SNR 约 1 dB），噪声会让这条测试测到素材而不是代码。
func TestOpusRoundTrip(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 1, BitrateBps: 128_000})
	dec := mustDecoder(t, OpusDecoderConfig{Channels: 1, PreSkip: enc.PreSkip()})

	const frames = 50
	stream := genTone(0, enc.FrameSamples()*frames, 1, 440, 0.5)
	packets := encodeFrames(t, enc, stream)

	var got []float32
	for i, pkt := range packets {
		f, err := dec.Decode(pkt, 0)
		if err != nil {
			t.Fatalf("Decode(%d): %v", i, err)
		}
		if f == nil {
			continue
		}
		got = append(got, f.PCM...)
	}

	best, lag := bestCorrelation(stream, got, enc.FrameSamples())
	if best < 0.95 {
		t.Errorf("best correlation %0.4f at lag %d, want >= 0.95", best, lag)
	}
	if lag > enc.FrameSamples() {
		t.Errorf("best lag %d exceeds one %d-sample frame: decoded stream is misaligned",
			lag, enc.FrameSamples())
	}
	t.Logf("correlation %0.4f at lag %d (pre-skip %d)", best, lag, enc.PreSkip())
}

// TestOpusDecoderPLC 验证丢包时补一帧而不是留空洞：空包触发 PLC，输出仍是
// 标准帧长，调用方可以把它当普通帧接在时间线上。
func TestOpusDecoderPLC(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 2, BitrateBps: 128_000})
	dec := mustDecoder(t, OpusDecoderConfig{Channels: 2, PreSkip: enc.PreSkip()})

	stream := genTone(0, enc.FrameSamples()*20, 2, 440, 0.5)
	packets := encodeFrames(t, enc, stream)

	// 先喂几个真实包，让解码器带上状态，PLC 才有东西可插值。
	for _, pkt := range packets[:5] {
		if _, err := dec.Decode(pkt, 0); err != nil {
			t.Fatalf("Decode: %v", err)
		}
	}

	f, err := dec.Decode(nil, 0)
	if err != nil {
		t.Fatalf("Decode(nil) for PLC: %v", err)
	}
	if f == nil {
		t.Fatal("Decode(nil) returned no frame, want a concealed frame")
	}
	if !f.PLC {
		t.Error("PLC flag not set on a concealed frame")
	}
	// pre-skip 只有 312 采样，前 5 包早就走完了，PLC 帧应是满的。
	want := enc.FrameSamples() * 2
	if len(f.PCM) != want {
		t.Errorf("PLC frame has %d samples, want %d", len(f.PCM), want)
	}
	if f.Duration != OpusFrameMs*1e6 {
		t.Errorf("PLC frame Duration=%v, want %v", f.Duration, OpusFrameMs*1e6)
	}
}

func TestOpusDecoderFEC(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 2, BitrateBps: 128_000})
	dec := mustDecoder(t, OpusDecoderConfig{Channels: 2, PreSkip: enc.PreSkip()})

	stream := genTone(0, enc.FrameSamples()*20, 2, 440, 0.5)
	packets := encodeFrames(t, enc, stream)

	for _, pkt := range packets[:5] {
		if _, err := dec.Decode(pkt, 0); err != nil {
			t.Fatalf("Decode: %v", err)
		}
	}

	// 用第 6 个包尝试恢复第 5 个（假设的丢失帧）。go-opus 在包里没有覆盖
	// 该时长的 FEC 数据时会自动退回 PLC，所以这里只要求产出满帧、不报错。
	f, err := dec.DecodeFEC(packets[5], 0)
	if err != nil {
		t.Fatalf("DecodeFEC: %v", err)
	}
	if len(f.PCM) != enc.FrameSamples()*2 {
		t.Errorf("FEC frame has %d samples, want %d", len(f.PCM), enc.FrameSamples()*2)
	}
}

func TestOpusDecoderResetRearmsPreSkip(t *testing.T) {
	enc := mustEncoder(t, OpusEncoderConfig{Channels: 1, BitrateBps: 128_000})
	preSkip := enc.PreSkip()
	stream := genTone(0, enc.FrameSamples()*10, 1, 440, 0.5)
	packets := encodeFrames(t, enc, stream)

	dec := mustDecoder(t, OpusDecoderConfig{Channels: 1, PreSkip: preSkip})

	first, err := dec.Decode(packets[0], 0)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := len(first.PCM); got != enc.FrameSamples()-preSkip {
		t.Errorf("first frame has %d samples, want %d (one frame minus pre-skip)",
			got, enc.FrameSamples()-preSkip)
	}

	// Reset 的语义是"开始一条新流"，所以 pre-skip 要重新裁一遍。
	dec.Reset()
	again, err := dec.Decode(packets[0], 0)
	if err != nil {
		t.Fatalf("Decode after Reset: %v", err)
	}
	if got := len(again.PCM); got != enc.FrameSamples()-preSkip {
		t.Errorf("first frame after Reset has %d samples, want %d", got, enc.FrameSamples()-preSkip)
	}
}

func TestOpusDecoderRelease(t *testing.T) {
	dec := mustDecoder(t, OpusDecoderConfig{})
	dec.Release()
	if _, err := dec.Decode([]byte{0x00}, 0); err == nil {
		t.Error("Decode after Release succeeded, want error")
	}
	if _, err := dec.DecodeFEC([]byte{0x00}, 0); err == nil {
		t.Error("DecodeFEC after Release succeeded, want error")
	}
	dec.Reset() // 不应 panic
}

// bestCorrelation 在 ref 上滑动 got，返回最高的归一化相关系数与对应的 lag。
func bestCorrelation(ref, got []float32, maxLag int) (best float64, bestLag int) {
	n := len(got)
	limit := len(ref) - n
	if limit > maxLag {
		limit = maxLag
	}
	for lag := 0; lag <= limit; lag++ {
		if c := correlation(ref[lag:lag+n], got); c > best {
			best, bestLag = c, lag
		}
	}
	return best, bestLag
}

func correlation(a, b []float32) float64 {
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

func BenchmarkOpusDecodeStereo(b *testing.B) {
	enc, err := NewOpusEncoder(OpusEncoderConfig{Channels: 2, BitrateBps: 128_000, Complexity: 5})
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Release()
	pcm := genTone(0, enc.FrameSamples(), 2, 440, 0.5)
	pkt, err := enc.Encode(pcm, 0)
	if err != nil {
		b.Fatal(err)
	}
	packet := append([]byte(nil), pkt.Data...)

	dec, err := NewOpusDecoder(OpusDecoderConfig{Channels: 2, PreSkip: enc.PreSkip()})
	if err != nil {
		b.Fatal(err)
	}
	defer dec.Release()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := dec.Decode(packet, 0); err != nil {
			b.Fatal(err)
		}
	}
}
