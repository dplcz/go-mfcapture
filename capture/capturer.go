package capture

// Capturer abstracts a screen capture source. Platform-specific implementations
// (DXGI Desktop Duplication on Windows, PipeWire on Linux, etc.) satisfy this
// interface so the encoding and session layers remain platform-agnostic.
type Capturer interface {
	Width() uint32
	Height() uint32
	AcquireFrame(timeoutMs uint32) (*CapturedFrame, error)
	Release()
}
