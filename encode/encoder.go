package encode

import (
	"time"

	"mfcapture/mf"
)

// EncodedFrame holds a single encoded output frame.
type EncodedFrame struct {
	Data       []byte
	Timestamp  time.Duration
	Duration   time.Duration
	IsKeyFrame bool
}

// FrameCallback is the application-layer callback that receives encoded frames.
type FrameCallback func(frame *EncodedFrame)

// Encoder abstracts a video encoder. Platform-specific implementations
// (Media Foundation MFT on Windows, VAAPI on Linux, etc.) satisfy this
// interface so the session layer remains platform-agnostic.
type Encoder interface {
	Encode(sample *mf.IMFSample) (*EncodedFrame, error)
	ForceKeyFrame() error
	SetBitrate(bps uint32) error
	Flush() ([]*EncodedFrame, error)
	Release()
}
