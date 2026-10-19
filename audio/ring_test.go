package audio

import (
	"encoding/binary"
	"testing"
	"time"
)

// rampS16 生成 sampleFrames 帧交错 S16 数据：第一声道依次写入
// start, start+1, ...，其余声道为 0。序号用来验证缓冲顺序和丢弃位置。
func rampS16(sampleFrames, channels, start int) []byte {
	ba := 2 * channels
	data := make([]byte, sampleFrames*ba)
	for i := 0; i < sampleFrames; i++ {
		binary.LittleEndian.PutUint16(data[i*ba:], uint16(start+i))
	}
	return data
}

// firstChannel 取出每个采样帧的第一声道，用于断言 rampS16 的连续性。
func firstChannel(data []byte, channels int) []int16 {
	ba := 2 * channels
	out := make([]int16, len(data)/ba)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*ba:]))
	}
	return out
}

func mustRing(t *testing.T, maxLatency time.Duration) *Ring {
	t.Helper()
	r, err := NewRing(testFormat(false), maxLatency)
	if err != nil {
		t.Fatalf("NewRing: %v", err)
	}
	return r
}

func TestNewRingRejectsBadFormat(t *testing.T) {
	if _, err := NewRing(Format{}, 0); err == nil {
		t.Error("NewRing accepted an invalid format")
	}
}

func TestRingCapacity(t *testing.T) {
	r := mustRing(t, 200*time.Millisecond)
	if got, want := r.Cap(), 9600; got != want {
		t.Errorf("Cap()=%d sample-frames, want %d (200 ms / 20 ms x 960)", got, want)
	}

	// 小于一帧的请求也至少给一帧，否则任何 Append 都放不下。
	small := mustRing(t, time.Millisecond)
	if got := small.Cap(); got != 960 {
		t.Errorf("Cap()=%d for a sub-frame latency, want 960", got)
	}
}

func TestRingAppendTake(t *testing.T) {
	r := mustRing(t, 200*time.Millisecond)

	dropped, err := r.Append(rampS16(960, 2, 0))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if dropped != 0 {
		t.Errorf("Append dropped %d samples, want 0", dropped)
	}
	if got := r.Available(); got != 960 {
		t.Fatalf("Available()=%d, want 960", got)
	}

	head, err := r.Take(480)
	if err != nil {
		t.Fatalf("Take(480): %v", err)
	}
	if got := firstChannel(head, 2)[0]; got != 0 {
		t.Errorf("first sample after Take(480)=%d, want 0", got)
	}

	tail, err := r.Take(480)
	if err != nil {
		t.Fatalf("Take(480): %v", err)
	}
	if got := firstChannel(tail, 2)[0]; got != 480 {
		t.Errorf("first sample of second half=%d, want 480", got)
	}
	if got := r.Available(); got != 0 {
		t.Errorf("Available()=%d after draining, want 0", got)
	}
}

func TestRingEmptyAppend(t *testing.T) {
	r := mustRing(t, 0)
	dropped, err := r.Append(nil)
	if err != nil || dropped != 0 {
		t.Errorf("Append(nil)=(%d, %v), want (0, nil)", dropped, err)
	}
	if got := r.Available(); got != 0 {
		t.Errorf("Available()=%d, want 0", got)
	}
}

func TestRingRejectsMisalignedAppend(t *testing.T) {
	r := mustRing(t, 0)
	if _, err := r.Append(make([]byte, 3)); err == nil {
		t.Error("Append accepted a 3-byte buffer with block align 4")
	}
	if got := r.Available(); got != 0 {
		t.Errorf("Available()=%d after a rejected append, want 0", got)
	}
}

func TestRingTakeErrors(t *testing.T) {
	r := mustRing(t, 0)
	if _, err := r.Take(1); err == nil {
		t.Error("Take(1) on an empty ring succeeded, want error")
	}
	if _, err := r.Take(-1); err == nil {
		t.Error("Take(-1) succeeded, want error")
	}
}

// TestRingWrapAround 强制 head 前移后写入越过缓冲末尾，验证压缩搬移后
// 数据顺序不变。
func TestRingWrapAround(t *testing.T) {
	r := mustRing(t, 40*time.Millisecond) // 2 帧容量

	if _, err := r.Append(rampS16(1440, 2, 0)); err != nil { // 1.5 帧
		t.Fatalf("Append: %v", err)
	}
	if _, err := r.Take(480); err != nil { // head 前移 0.5 帧
		t.Fatalf("Take: %v", err)
	}
	if _, err := r.Append(rampS16(960, 2, 1440)); err != nil { // tail 越过末尾
		t.Fatalf("Append: %v", err)
	}
	if got := r.Available(); got != 1920 {
		t.Fatalf("Available()=%d, want 1920", got)
	}

	first, err := r.Take(960)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := firstChannel(first, 2); got[0] != 480 || got[len(got)-1] != 1439 {
		t.Errorf("first frame spans %d..%d, want 480..1439", got[0], got[len(got)-1])
	}

	second, err := r.Take(960)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := firstChannel(second, 2); got[0] != 1440 || got[len(got)-1] != 2399 {
		t.Errorf("second frame spans %d..%d, want 1440..2399", got[0], got[len(got)-1])
	}
}

func TestRingDropsOldestOnOverflow(t *testing.T) {
	r := mustRing(t, 40*time.Millisecond) // 2 帧容量

	if _, err := r.Append(rampS16(1440, 2, 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	dropped, err := r.Append(rampS16(960, 2, 1440)) // 1.5 + 1 = 2.5 帧，溢出半帧
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if dropped != 960 {
		t.Errorf("dropped %d sample-frames, want one whole 960 frame", dropped)
	}
	if got := r.Available(); got != 1440 {
		t.Fatalf("Available()=%d, want 1440", got)
	}
	if got := r.DroppedSamples(); got != 960 {
		t.Errorf("DroppedSamples()=%d, want 960", got)
	}

	head, err := r.Take(960)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := firstChannel(head, 2)[0]; got != 960 {
		t.Errorf("head sample after dropping one frame=%d, want 960", got)
	}
}

func TestRingOversizedAppendKeepsNewest(t *testing.T) {
	r := mustRing(t, 40*time.Millisecond) // 2 帧容量

	dropped, err := r.Append(rampS16(2880, 2, 0)) // 3 帧
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if dropped != 960 {
		t.Errorf("dropped %d sample-frames, want 960 (one leading frame)", dropped)
	}
	if got := r.Available(); got != 1920 {
		t.Fatalf("Available()=%d, want 1920", got)
	}
	head, err := r.Take(960)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := firstChannel(head, 2)[0]; got != 960 {
		t.Errorf("kept data starts at %d, want 960 (newest two frames)", got)
	}
}

func TestRingDrop(t *testing.T) {
	r := mustRing(t, 0)
	if _, err := r.Append(rampS16(1920, 2, 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if got := r.Drop(480); got != 480 {
		t.Errorf("Drop(480)=%d, want 480", got)
	}
	if got := r.Available(); got != 1440 {
		t.Errorf("Available()=%d, want 1440", got)
	}
	if got := r.Drop(10000); got != 1440 {
		t.Errorf("Drop(10000)=%d, want 1440 (clamped)", got)
	}
	if got := r.Available(); got != 0 {
		t.Errorf("Available()=%d, want 0", got)
	}
	if got := r.DroppedSamples(); got != 0 {
		t.Errorf("explicit Drop counted as overflow: DroppedSamples()=%d", got)
	}
}

func TestRingReset(t *testing.T) {
	r := mustRing(t, 40*time.Millisecond)                    // 2 帧容量
	if _, err := r.Append(rampS16(2880, 2, 0)); err != nil { // 3 帧，触发溢出
		t.Fatalf("Append: %v", err)
	}
	if got := r.DroppedSamples(); got == 0 {
		t.Fatal("test setup did not trigger an overflow")
	}
	r.Reset()
	if got := r.Available(); got != 0 {
		t.Errorf("Available()=%d after Reset, want 0", got)
	}
	if got := r.DroppedSamples(); got != 0 {
		t.Errorf("DroppedSamples()=%d after Reset, want 0", got)
	}
}
