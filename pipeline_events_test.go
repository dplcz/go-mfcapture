package mfcapture

import (
	"errors"
	"testing"

	"mfcapture/capture"
	"mfcapture/encode"
)

// eventSink 记录事件序列，用于验证暂停/恢复边沿。
type eventSink struct{ events []Event }

func (*eventSink) OnFrame(*encode.EncodedFrame) {}
func (*eventSink) OnCursor(*capture.CursorInfo) {}
func (s *eventSink) OnEvent(e Event)            { s.events = append(s.events, e) }

func TestCaptureStateEdgesReportOnce(t *testing.T) {
	sink := &eventSink{}
	p := &Pipeline{sink: sink}
	cause := errors.New("desktop duplication temporarily unavailable")

	p.setCapturePaused(cause)
	p.setCapturePaused(cause) // 重试循环每 250ms 调一次，不能重复上报
	p.setCaptureResumed()
	p.setCaptureResumed()

	if len(sink.events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(sink.events), sink.events)
	}
	if got := sink.events[0].Kind; got != EventCapturePaused {
		t.Fatalf("first event kind = %v, want EventCapturePaused", got)
	}
	if !errors.Is(sink.events[0].Err, cause) {
		t.Fatalf("paused event err = %v, want %v", sink.events[0].Err, cause)
	}
	if got := sink.events[1].Kind; got != EventCaptureResumed {
		t.Fatalf("second event kind = %v, want EventCaptureResumed", got)
	}
}

func TestCaptureResumeWithoutPauseIsSilent(t *testing.T) {
	sink := &eventSink{}
	p := &Pipeline{sink: sink}
	p.setCaptureResumed()
	if len(sink.events) != 0 {
		t.Fatalf("events = %+v, want none", sink.events)
	}
}
