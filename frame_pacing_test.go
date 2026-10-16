package mfcapture

import "testing"

const hnsPerMs = int64(10_000)

func TestFrameLimiterUnlimited(t *testing.T) {
	l := newFrameLimiter(0)
	for i := range 10 {
		if !l.allow(int64(i)) {
			t.Fatalf("frame %d dropped with MaxFPS=0", i)
		}
	}
}

// run 把 count 帧（间隔 srcHns，叠加 jitter 循环）喂给限流器，返回写入的帧数。
func run(l *frameLimiter, count int, srcHns int64, jitter []int64) int {
	kept := 0
	for i := range count {
		if l.allow(int64(i)*srcHns + jitter[i%len(jitter)]) {
			kept++
		}
	}
	return kept
}

// 59Hz 来源 + 较大抖动（峰峰值 2.3ms）在 MaxFPS=60 下不应丢帧。
func TestFrameLimiterNearRateSourceWithJitter(t *testing.T) {
	jitter := []int64{0, 7000, -9000, 12000, -3000, 9000, -11000, 4000}
	if kept := run(newFrameLimiter(60), 590, int64(10_000_000)/59, jitter); kept < 590*99/100 {
		t.Errorf("kept %d of 590 frames, want >= 99%%", kept)
	}
}

// 高刷来源按上限降采样，写入帧率应接近上限而不是减半。
func TestFrameLimiterDownsamples(t *testing.T) {
	jitter := []int64{0, 3000, -2000, 1000}
	// 144Hz 来源 10 秒 → 60fps 上限：期望约 600 帧。
	kept := run(newFrameLimiter(60), 1440, int64(10_000_000)/144, jitter)
	if kept < 590 || kept > 610 {
		t.Errorf("kept %d frames from 144Hz source over 10s, want ~600", kept)
	}
	// 120Hz 来源：恰好每隔一帧。
	kept = run(newFrameLimiter(60), 1200, int64(10_000_000)/120, []int64{0})
	if kept < 590 || kept > 610 {
		t.Errorf("kept %d frames from 120Hz source over 10s, want ~600", kept)
	}
}

// 画面静止后恢复：重新对齐，不会连续放行补帧。
func TestFrameLimiterResyncAfterIdle(t *testing.T) {
	l := newFrameLimiter(60)
	l.allow(0)
	resume := 5_000 * hnsPerMs // 静止 5 秒
	if !l.allow(resume) {
		t.Fatal("first frame after idle dropped")
	}
	if l.allow(resume + 2*hnsPerMs) {
		t.Error("frame 2ms after resume allowed; limiter burst after idle")
	}
	if !l.allow(resume + 17*hnsPerMs) {
		t.Error("frame 17ms after resume dropped")
	}
}
