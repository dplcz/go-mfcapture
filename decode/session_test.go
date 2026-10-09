package decode

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestSessionReleaseMarshalsToRunThread 验证 Session 产出的帧的归还路径：
// 渲染线程调用 Release 不会就地释放 COM 对象，而是把帧投递回 Run 所在线程。
// 这是本次线程模型改造的核心机制，不需要 COM 就能测。
func TestSessionReleaseMarshalsToRunThread(t *testing.T) {
	s, err := NewSession(SessionConfig{}, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	var calls atomic.Int64
	f := &DecodedFrame{release: func() { calls.Add(1) }}
	s.bind(f)
	if s.inFlight != 1 {
		t.Fatalf("inFlight = %d, want 1", s.inFlight)
	}

	// 模拟渲染线程：在另一个 goroutine 上归还。
	done := make(chan struct{})
	go func() {
		f.Release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Release 阻塞了：它不应在渲染线程等待解码线程")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("Release 在渲染线程就地释放了 %d 次，应等 Run 线程排空", n)
	}
	if len(s.relCh) != 1 {
		t.Fatalf("待归还队列长度 = %d, want 1", len(s.relCh))
	}

	// 这里直接调 drainReturns 代表 Run 的循环体。
	s.drainReturns()
	if n := calls.Load(); n != 1 {
		t.Fatalf("实际释放次数 = %d, want 1", n)
	}
	if s.inFlight != 0 {
		t.Fatalf("inFlight = %d, want 0", s.inFlight)
	}

	// 重复 Release 必须是安全的空操作（Session 路径下归还是异步的，
	// 没有这层保护会把同一帧归还两次，引用计数直接下溢）。
	f.Release()
	f.Release()
	if n := calls.Load(); n != 1 {
		t.Fatalf("重复 Release 后又释放了 %d 次", n-calls.Load())
	}
}

// TestSessionReturnFallsBackWhenQueueFull 验证待归还队列满（或 Run 已返回）时
// 就地释放，而不是把帧丢弃造成泄漏。
func TestSessionReturnFallsBackWhenQueueFull(t *testing.T) {
	s, err := NewSession(SessionConfig{}, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	var calls atomic.Int64
	total := maxPendingReturns + 4 // 故意超过待归还队列容量
	frames := make([]*DecodedFrame, 0, total)
	for i := 0; i < total; i++ {
		f := &DecodedFrame{release: func() { calls.Add(1) }}
		s.bind(f)
		frames = append(frames, f)
	}
	for _, f := range frames {
		f.Release() // 队列装不下的那几帧当场就地释放，不会丢
	}
	if got, want := calls.Load(), int64(total-maxPendingReturns); got != want {
		t.Fatalf("超载部分释放次数 = %d, want %d", got, want)
	}
	if s.inFlight != maxPendingReturns {
		t.Fatalf("inFlight = %d, want %d", s.inFlight, maxPendingReturns)
	}

	s.drainReturns()
	if got, want := calls.Load(), int64(total); got != want {
		t.Fatalf("总释放次数 = %d, want %d（一帧都不能漏）", got, want)
	}
	if s.inFlight != 0 {
		t.Fatalf("inFlight = %d, want 0", s.inFlight)
	}
}

// TestSessionSubmitAndClose 验证 Submit 的入队语义：复制 AU、队列满返回 false、
// Close 之后不再接收。
func TestSessionSubmitAndClose(t *testing.T) {
	s, err := NewSession(SessionConfig{QueueSize: 1}, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	au := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x01}
	if !s.Submit(au, 0, time.Second/30) {
		t.Fatal("第一次 Submit 失败")
	}
	// Submit 必须复制一份：改掉调用方的缓冲区不能影响队列里那份。
	au[4] = 0x41
	got := <-s.auCh
	if got.data[4] != 0x65 {
		t.Fatalf("Submit 没有复制 AU，队列里被改成了 %v", got.data)
	}

	if !s.Submit(au, 0, time.Second/30) {
		t.Fatal("队列已排空，第二次 Submit 应成功")
	}
	if s.Submit(au, 0, time.Second/30) {
		t.Fatal("队列满时 Submit 应返回 false")
	}
	if s.Submit(nil, 0, time.Second/30) {
		t.Fatal("空 AU 不应被接收")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-s.auCh // 排空
	if s.Submit(au, 0, time.Second/30) {
		t.Fatal("Close 之后 Submit 应返回 false")
	}
}

// TestSessionEndOfStreamIsIdempotent 验证多次调用 EndOfStream 不会 panic。
func TestSessionEndOfStreamIsIdempotent(t *testing.T) {
	s, err := NewSession(SessionConfig{}, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.EndOfStream()
	s.EndOfStream()
	select {
	case <-s.eosCh:
	default:
		t.Fatal("EndOfStream 没有关闭 eosCh")
	}
}

// countSink 统计会话上报的帧与事件。
type countSink struct {
	frames   atomic.Int64
	ready    atomic.Bool
	degraded atomic.Bool
	errs     atomic.Int64
}

func (s *countSink) OnFrame(f *DecodedFrame) {
	s.frames.Add(1)
	f.Release()
}

func (s *countSink) OnEvent(e Event) {
	switch e.Kind {
	case EventReady:
		s.ready.Store(true)
	case EventDegraded:
		s.degraded.Store(true)
	case EventDecodeError:
		s.errs.Add(1)
	}
}

// TestSessionRunLifecycle is a manual integration test: it builds a real decoder
// (COM/MF init happens inside Run) and shuts the session down with no input,
// verifying the whole LockOSThread + CoInitializeEx/MFStartup + teardown path.
// Run with: MFCAPTURE_MANUAL_TEST=1 go test -run TestSessionRunLifecycle -v ./decode
func TestSessionRunLifecycle(t *testing.T) {
	if os.Getenv("MFCAPTURE_MANUAL_TEST") != "1" {
		t.Skip("set MFCAPTURE_MANUAL_TEST=1 to run this manual integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var sink countSink
	sess, err := NewSession(SessionConfig{}, &sink)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	sess.EndOfStream() // 没有输入：Run 应当排空解码器后返回 nil
	if err := sess.Run(ctx); err != nil {
		t.Fatalf("Session.Run: %v", err)
	}
	if !sink.ready.Load() {
		t.Fatal("没有收到 EventReady")
	}
	if n := sink.frames.Load(); n != 0 {
		t.Fatalf("没有输入却解出了 %d 帧", n)
	}
	t.Logf("codec ready, format = %+v", sess.Format())
}
