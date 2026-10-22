package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"mfcapture"
	"mfcapture/audio"
	"mfcapture/encode"
)

func testAudioDesc() mfcapture.AudioDesc {
	return mfcapture.AudioDesc{
		Codec:      "opus",
		SampleRate: audio.OpusSampleRate,
		Channels:   2,
		FrameMs:    audio.OpusFrameMs,
		PreSkip:    audio.DefaultPreSkip,
	}
}

func encodedAudioFrame(ts time.Duration) *audio.EncodedFrame {
	return &audio.EncodedFrame{
		Data:      []byte{0x01},
		Timestamp: ts,
		Duration:  time.Duration(audio.OpusFrameMs) * time.Millisecond,
	}
}

// TestAudioSinkDropsPreRoll 守住 P6 的音画对齐规则：视频首帧之前的音频包
// 必须被丢弃，之后的第一帧成为 Ogg 文件里的第一个包。
func TestAudioSinkDropsPreRoll(t *testing.T) {
	var buf bytes.Buffer
	sink := &audioSink{
		w:      bufio.NewWriter(&buf),
		serial: 1,
		clock:  &videoClock{},
	}
	sink.start(testAudioDesc())

	sink.OnAudioFrame(encodedAudioFrame(20 * time.Millisecond))
	sink.OnAudioFrame(encodedAudioFrame(40 * time.Millisecond))
	if sink.preRoll != 2 || sink.frames != 0 {
		t.Fatalf("before video start: preRoll=%d frames=%d, want 2/0", sink.preRoll, sink.frames)
	}

	sink.clock.mark(100 * time.Millisecond)
	sink.OnAudioFrame(encodedAudioFrame(100 * time.Millisecond))
	sink.OnAudioFrame(encodedAudioFrame(120 * time.Millisecond))
	if sink.preRoll != 2 || sink.frames != 2 {
		t.Fatalf("after video start: preRoll=%d frames=%d, want 2/2", sink.preRoll, sink.frames)
	}
	if sink.firstPTS != 100*time.Millisecond {
		t.Errorf("first PTS = %v, want 100ms", sink.firstPTS)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(buf.String(), "OpusHead") {
		t.Error("output is not an Ogg Opus stream")
	}
	if !strings.Contains(buf.String(), "OpusTags") {
		t.Error("output has no OpusTags page")
	}
}

// TestAudioSinkWithoutVideoWritesEverything 覆盖 -no-video：没有视频时钟时
// 不能丢任何包。
func TestAudioSinkWithoutVideoWritesEverything(t *testing.T) {
	var buf bytes.Buffer
	sink := &audioSink{w: bufio.NewWriter(&buf), serial: 2}
	sink.start(testAudioDesc())
	for i := range 5 {
		sink.OnAudioFrame(encodedAudioFrame(time.Duration(i) * 20 * time.Millisecond))
	}
	if sink.preRoll != 0 || sink.frames != 5 {
		t.Errorf("preRoll=%d frames=%d, want 0/5", sink.preRoll, sink.frames)
	}
}

func TestVideoSinkPublishesFirstPTS(t *testing.T) {
	var buf bytes.Buffer
	clock := &videoClock{}
	sink := &videoSink{w: bufio.NewWriter(&buf), clock: clock}

	sink.OnFrame(&encode.EncodedFrame{Data: []byte("first"), Timestamp: 5 * time.Millisecond})
	sink.OnFrame(&encode.EncodedFrame{Data: []byte("second"), Timestamp: 25 * time.Millisecond})

	start, ok := clock.start()
	if !ok || start != 5*time.Millisecond {
		t.Errorf("clock start = %v (ok=%v), want 5ms", start, ok)
	}
	if sink.firstPTS != 5*time.Millisecond || sink.lastPTS != 25*time.Millisecond {
		t.Errorf("first/last PTS = %v/%v, want 5ms/25ms", sink.firstPTS, sink.lastPTS)
	}
}
