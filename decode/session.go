package decode

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	d3d "github.com/kirides/go-d3d"
	"golang.org/x/sys/windows"

	"mfcapture/mf"
)

const (
	// defaultQueueSize 是待解码访问单元队列的默认深度。
	defaultQueueSize = 32
	// maxPendingReturns 是"待归还帧"队列的深度。同时在飞的帧数受解码器
	// surface 池限制（个位数），16 足够，不会成为瓶颈。
	maxPendingReturns = 16
	// flushReturnWait 排空阶段等待渲染线程归还帧的上限。渲染器卡住时不能让
	// Run 永远不返回，超时就放弃剩余帧。
	flushReturnWait = 2 * time.Second
	// maxFlushRounds 排空循环的次数上限，纯防御性。
	maxFlushRounds = 256
)

// OutputFormat 描述解码器协商后的输出，供上层初始化渲染目标。
type OutputFormat struct {
	Subtype  windows.GUID
	Width    uint32
	Height   uint32
	Stride   int32
	Hardware bool // 是否真的启用了 DXVA 硬件解码
}

// EventKind 标识解码会话主动上报的状态变化。
type EventKind uint8

const (
	// EventReady 在第一个 OnFrame 之前触发一次，此时 Format 已可用。
	// 注意此时宽高可能还是 0——解码器要等 SPS 才拿得到真实尺寸，真实尺寸
	// 通过随后的 EventFormatChanged 上报。
	EventReady EventKind = iota
	// EventFormatChanged 码流分辨率/像素格式变化后触发（解码器内部已完成
	// stream change 重协商），上层应按新 Format 重建渲染目标。
	EventFormatChanged
	// EventDegraded DXVA 出错、解码器被重建为纯软件解码时触发。Err 是触发
	// 回退的那个错误。会话继续跑，不会因此停止。
	EventDegraded
	// EventDecodeError 单个访问单元解码失败时触发。会话继续跑。
	EventDecodeError
)

// Event 是解码会话主动上报的状态变化。
type Event struct {
	Kind   EventKind
	Format OutputFormat // EventReady / EventFormatChanged 时有效
	Err    error        // EventDegraded / EventDecodeError 时有效
}

// Sink 是解码会话向上层交付数据的通道。由上层实现。
//
// 会话不做队列、不丢帧、不限流——它只保证"解出一帧就调一次 OnFrame"。
// 是否阻塞、队列多深、满了丢什么，全部是上层自己的策略。
type Sink interface {
	// OnFrame 在 Run() 所在线程内同步调用。上层实现若在此阻塞，等于自己选择
	// 让解码停下来（DXVA 下表现为解码器 surface 池占满、背压）。
	OnFrame(f *DecodedFrame)
	// OnEvent 上报状态变化。OnEvent(EventReady) 保证早于第一个 OnFrame。
	OnEvent(e Event)
}

// nopSink 供基准测试/调试使用；上层不传 Sink 时用它兜底，使回调点无需判空。
type nopSink struct{}

func (nopSink) OnFrame(*DecodedFrame) {}
func (nopSink) OnEvent(Event)         {}

// SessionConfig 配置一个解码会话。
type SessionConfig struct {
	// H264DecoderConfig 是底层解码器的配置。Device/DeviceCtx 传了就会复用，
	// 使解码器输出纹理与渲染设备同属一个 D3D11 Device（零拷贝渲染）。
	H264DecoderConfig
	// QueueSize 是待解码访问单元的队列深度，0 = 32。
	// 队列满时 Submit 返回 false（该 AU 未被接收）——丢 P 帧还是丢 IDR，
	// 由上层决定，本包不替你选。
	QueueSize int
}

// Session 是一条"H264 字节 → 像素帧"的解码会话。
//
// 线程模型（这是本类型最重要的约定）：
//   - NewSession 只做纯构造，不碰 COM，可在任意 goroutine 调用。
//   - Run 会调用 runtime.LockOSThread() 把调用方 goroutine 绑定到 OS 线程，并在该
//     线程上完成 COM/MF 初始化、建解码器、跑循环、清理，全程不换线程。COM/MF 的
//     初始化与释放都发生在 Run 内部，不会出现"初始化在 A 线程、释放在 B 线程"的
//     公寓错配。
//   - Submit / EndOfStream / Format 可在任意 goroutine 调用；AU 通过队列投递到 Run
//     所在线程解码，绝不跨线程直接调 COM。
//   - DecodedFrame.Release() 也可以在任意（渲染）线程调用：它不会就地释放 COM 对象，
//     而是把帧投递回 Run 所在线程归还——DXVA 下释放 IMFSample/纹理必须和解码同线程。
//   - 停止会话请 cancel 传给 Run 的 ctx，或调用 EndOfStream() 声明输入结束；
//     Close 只需在 Run 返回后调用一次。
//
// 底层 H264MFTDecoder 仍然是可单独使用的组件，但它**不绑定线程**：直接用就必须
// 由调用方自己保证 CoInitializeEx + MFStartup + LockOSThread 且全程不换线程。
// 需要"我什么都不管"的调用方请用本类型。
type Session struct {
	cfg  SessionConfig
	sink Sink

	auCh    chan accessUnit
	relCh   chan *DecodedFrame
	eosCh   chan struct{}
	eosOnce sync.Once
	stopped atomic.Bool
	closed  atomic.Bool

	formatMu sync.RWMutex
	format   OutputFormat

	// 以下字段只在 Run() 所在 goroutine 内访问。
	dec      *H264MFTDecoder
	inFlight int // 已交付、但渲染线程还没归还的帧数
}

type accessUnit struct {
	data []byte
	ts   time.Duration
	dur  time.Duration
}

// NewSession 构造一个解码会话。不做任何 COM/MF 调用。
// sink 为 nil 时所有回调变成 no-op（仅建议基准测试使用）。
func NewSession(cfg SessionConfig, sink Sink) (*Session, error) {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if sink == nil {
		sink = nopSink{}
	}
	return &Session{
		cfg:   cfg,
		sink:  sink,
		auCh:  make(chan accessUnit, cfg.QueueSize),
		relCh: make(chan *DecodedFrame, maxPendingReturns),
		eosCh: make(chan struct{}),
	}, nil
}

// Submit 投递一个访问单元（Annex-B 字节，含起始码）。可在任意 goroutine 调用；
// 解码在 Run 所在线程异步进行，所以本方法不阻塞在解码上。
//
// 返回 false 表示会话已关闭或队列已满——该 AU 没有被接收，上层自己决定是重投、
// 还是丢弃（丢 P 帧通常安全，丢 IDR 会让后续帧解到下一个关键帧为止）。
//
// au 会被复制一份，调用方可以在返回后自由复用/归还自己的缓冲区。
func (s *Session) Submit(au []byte, ts, dur time.Duration) bool {
	if s.closed.Load() || len(au) == 0 {
		return false
	}
	buf := make([]byte, len(au))
	copy(buf, au)
	select {
	case s.auCh <- accessUnit{data: buf, ts: ts, dur: dur}:
		return true
	default:
		return false
	}
}

// EndOfStream 声明输入结束。Run 会在队列排空后 flush 解码器并返回 nil。
// 可重复调用，只有第一次生效。
func (s *Session) EndOfStream() {
	s.eosOnce.Do(func() { close(s.eosCh) })
}

// Format 返回当前输出格式，可在任意 goroutine 调用。
// 在收到 EventReady 之前返回零值（宽高可能仍为 0，见 EventReady 的说明）。
func (s *Session) Format() OutputFormat {
	s.formatMu.RLock()
	defer s.formatMu.RUnlock()
	return s.format
}

// Close 标记会话已关闭，之后的 Submit 变成 no-op。
// 应在 Run 返回之后调用；Run 内部的资源释放由 Run 自己完成，Close 不释放 COM 资源。
// 注意：队列通道不会 close，避免"先关闭后投递"导致 panic。
func (s *Session) Close() error {
	s.closed.Store(true)
	return nil
}

// Run 在调用方 goroutine 上运行整个解码会话，直到 ctx 被取消或输入结束。它是阻塞的。
//
// 返回的 error 为 nil（EndOfStream 后正常结束）、ctx.Err()（被取消），
// 或 COM/MF/解码器创建阶段的致命错误。单帧解码失败不会让 Run 返回，
// 而是通过 EventDecodeError 上报。
func (s *Session) Run(ctx context.Context) error {
	// COM/MF 的初始化与释放全部收在本函数内，保证同线程进出。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if hr := mf.CoInitializeEx(); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("CoInitializeEx: %w", d3d.HRESULT(hr))
	}
	defer mf.CoUninitialize()

	if hr := mf.MFStartup(); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("MFStartup: %w", d3d.HRESULT(hr))
	}
	defer mf.MFShutdown()

	dec, err := NewH264MFTDecoder(s.cfg.H264DecoderConfig)
	if err != nil {
		return fmt.Errorf("NewH264MFTDecoder: %w", err)
	}
	defer dec.Release()
	s.dec = dec

	// 同步 MFT 背压时会在 retry 循环里 sleep，而这时恰恰最需要有人归还已渲染的
	// 帧（surface 池空了才会背压）。把归还钩子挂进去，否则重试白等 50 次。
	dec.stallHook = s.drainReturns
	dec.degradeHook = func(cause error) {
		s.sink.OnEvent(Event{Kind: EventDegraded, Err: cause, Format: s.Format()})
	}

	// Run 返回前把还没归还的帧就地清掉，并让之后的 Release 走就地释放。
	defer func() {
		s.stopped.Store(true)
		s.drainReturns()
	}()

	s.publishFormat(dec, true)

	for {
		if err := ctx.Err(); err != nil {
			return s.drainDecoder(err)
		}
		select {
		case au := <-s.auCh:
			s.decodeAU(dec, au)
		case f := <-s.relCh:
			// 渲染线程归还的帧：在解码线程上真正释放 COM 对象。
			s.releaseNow(f)
		case <-s.eosCh:
			if len(s.auCh) == 0 {
				return s.drainDecoder(nil)
			}
		case <-ctx.Done():
			return s.drainDecoder(ctx.Err())
		}
	}
}

// decodeAU 解码一个访问单元并把产出的帧交给上层。只能在 Run 所在 goroutine 调用。
func (s *Session) decodeAU(dec *H264MFTDecoder, au accessUnit) {
	frames, err := dec.Decode(au.data, au.ts, au.dur)
	for _, f := range frames {
		s.bind(f)
		s.sink.OnFrame(f)
	}
	s.publishFormat(dec, false)
	if err != nil {
		// 帧已经尽量交付了，错误单独上报，会话继续跑。
		s.sink.OnEvent(Event{Kind: EventDecodeError, Err: err})
	}
}

// publishFormat 在格式变化时上报。ready=true 表示这是首次（EventReady）。
func (s *Session) publishFormat(dec *H264MFTDecoder, ready bool) {
	sub, w, h, stride := dec.OutputFormat()
	f := OutputFormat{Subtype: sub, Width: w, Height: h, Stride: stride, Hardware: dec.IsHardware}

	s.formatMu.RLock()
	same := f == s.format
	s.formatMu.RUnlock()
	if !ready && same {
		return
	}
	s.formatMu.Lock()
	s.format = f
	s.formatMu.Unlock()

	kind := EventFormatChanged
	if ready {
		kind = EventReady
	}
	s.sink.OnEvent(Event{Kind: kind, Format: f})
}

// bind 把帧的归还路径接管到本会话：之后 Release() 不再在渲染线程就地释放
// COM 对象，而是投递回 Run 所在线程。CPU 路径（release 为 nil）无需接管。
func (s *Session) bind(f *DecodedFrame) {
	if f == nil || f.release == nil {
		return
	}
	f.owner = s
	s.inFlight++
}

// returnFrame 由 DecodedFrame.Release 在任意（渲染）线程调用：把帧排进归还队列。
// 队列满或 Run 已返回时就地释放——漏一个引用也比泄漏强。
func (s *Session) returnFrame(f *DecodedFrame) {
	if f == nil || f.release == nil {
		return
	}
	if s.stopped.Load() {
		s.releaseNow(f)
		return
	}
	select {
	case s.relCh <- f:
	default:
		s.releaseNow(f)
	}
}

// drainReturns 排空归还队列。只能在 Run 所在 goroutine 调用。
func (s *Session) drainReturns() {
	for {
		select {
		case f := <-s.relCh:
			s.releaseNow(f)
		default:
			return
		}
	}
}

// releaseNow 真正释放一帧持有的解码器资源。只能在 Run 所在 goroutine 调用。
func (s *Session) releaseNow(f *DecodedFrame) {
	if f == nil {
		return
	}
	rel := f.release
	f.release = nil
	f.owner = nil
	f.Texture = nil
	if rel != nil {
		rel()
	}
	if s.inFlight > 0 {
		s.inFlight--
	}
}

// drainDecoder 排空解码器缓冲后返回 cause。ctx 取消或输入结束时调用。
func (s *Session) drainDecoder(cause error) error {
	dec := s.dec
	for round := 0; round < maxFlushRounds; round++ {
		s.drainReturns()

		tail, err := dec.Flush()
		for _, f := range tail {
			s.bind(f)
			s.sink.OnFrame(f)
		}
		if err != nil {
			s.sink.OnEvent(Event{Kind: EventDecodeError, Err: err})
			return cause
		}
		if len(tail) > 0 {
			continue
		}
		if s.inFlight == 0 {
			return cause // 真的排空了
		}
		// 还有帧在渲染线程手上：surface 池没归还前 Flush 只能给 0 帧，
		// 所以等一次归还再继续，别把"池满"误判成"排空"。
		select {
		case f := <-s.relCh:
			s.releaseNow(f)
		case <-time.After(flushReturnWait):
			return cause
		}
	}
	return cause
}
