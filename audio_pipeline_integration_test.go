//go:build windows && integration

package mfcapture

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAudioPipelineSmoke 用真实 WASAPI 端点跑 1 秒，验证 50 pps 的交付节奏。
// 系统静音时 WASAPI 可能只给 SILENT 包，管线本身会补静音，因此这里不检查
// 音频内容，只检查包率、时间戳与统计。
func TestAudioPipelineSmoke(t *testing.T) {
	sink := &collectSink{}
	p, err := NewAudioPipeline(AudioPipelineConfig{Epoch: time.Now()}, sink)
	if err != nil {
		t.Fatalf("NewAudioPipeline: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = p.Run(ctx)
	p.Close()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Skipf("WASAPI capture unavailable: %v", err)
	}

	frames, events := sink.snapshot()
	if len(events) == 0 || events[0].Kind != AudioDescReady {
		t.Fatalf("no AudioDescReady event (events=%d)", len(events))
	}
	desc := events[0].Desc
	t.Logf("desc: %+v; frames=%d; stats=%+v", desc, len(frames), p.Stats())
	if desc.SampleRate != 48000 || desc.FrameMs != 20 || desc.PreSkip <= 0 {
		t.Errorf("unexpected desc: %+v", desc)
	}
	if len(frames) < 40 || len(frames) > 60 {
		t.Errorf("delivered %d frames in 1s, want about 50", len(frames))
	}
	for i := 1; i < len(frames); i++ {
		if frames[i].Timestamp < frames[i-1].Timestamp {
			t.Errorf("frame %d PTS %v went backwards from %v", i, frames[i].Timestamp, frames[i-1].Timestamp)
		}
	}
}
