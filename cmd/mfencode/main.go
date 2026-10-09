package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"mfcapture"
	"mfcapture/capture"
	"mfcapture/encode"
)

// sink 是应用层的一种极简实现：把帧直接写文件。
// 真实应用会在这里改成"入队 + 满则丢 P 帧保 IDR"——队列策略与 mfcapture 无关。
type sink struct {
	f *os.File
}

func (s *sink) OnFrame(frame *encode.EncodedFrame) {
	if _, err := s.f.Write(frame.Data); err != nil {
		fmt.Fprintln(os.Stderr, "write error:", err)
	}
}

func (s *sink) OnCursor(*capture.CursorInfo) {}

func (s *sink) OnEvent(e mfcapture.Event) {
	switch e.Kind {
	case mfcapture.EventDescReady:
		fmt.Fprintf(os.Stderr, "desc ready: %dx%d fps=%d/%d format=%d\n",
			e.Desc.Width, e.Desc.Height, e.Desc.FPSNum, e.Desc.FPSDen, e.Desc.StreamFormat)
	case mfcapture.EventResolutionChanged:
		fmt.Fprintf(os.Stderr, "resolution changed: %dx%d\n", e.Desc.Width, e.Desc.Height)
	case mfcapture.EventEncoderDegraded:
		fmt.Fprintln(os.Stderr, "warning: no hardware H264 encoder, using software fallback")
	case mfcapture.EventCaptureError:
		fmt.Fprintln(os.Stderr, "capture error:", e.Err)
	}
}

func main() {
	duration := flag.Duration("duration", 5*time.Second, "recording duration")
	output := flag.String("o", "out.h264", "output raw H264 Annex B file path")
	bitrate := flag.Uint("bitrate", 8_000_000, "bitrate in bits per second")
	monitor := flag.Uint("monitor", 0, "monitor index (see -list)")
	maxFPS := flag.Uint("maxfps", 30, "cap on frames written per second (0 = unlimited)")
	gopSize := flag.Uint("gop", 30, "GOP size (keyframe interval in frames)")
	list := flag.Bool("list", false, "list available monitors and exit")
	flag.Parse()

	if *list {
		monitors, err := capture.EnumMonitors()
		if err != nil {
			fmt.Fprintln(os.Stderr, "EnumMonitors:", err)
			os.Exit(1)
		}
		for _, m := range monitors {
			fmt.Printf("[%d] %-16s %dx%d at (%d,%d) primary=%v attached=%v\n",
				m.Index, m.Name, m.Width, m.Height, m.Left, m.Top, m.Primary, m.Attached)
		}
		return
	}

	f, err := os.Create(*output)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create output file:", err)
		os.Exit(1)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	p, err := mfcapture.NewPipeline(mfcapture.PipelineConfig{
		Output:     *monitor,
		BitrateBps: uint32(*bitrate),
		MaxFPS:     uint32(*maxFPS),
		LowLatency: true,
		GOPSize:    uint32(*gopSize),
	}, &sink{f: f})
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline init failed:", err)
		os.Exit(1)
	}
	defer p.Close()

	fmt.Fprintf(os.Stderr, "mfencode: streaming H264 to %s (duration=%s, bitrate=%d, gop=%d, maxfps=%d)\n",
		*output, *duration, *bitrate, *gopSize, *maxFPS)

	runErr := p.Run(ctx)
	st := p.Stats()
	fmt.Fprintf(os.Stderr, "mfencode: done. %d frames (%d keyframes), %d bytes, hw=%v, last encode %dus\n",
		st.Frames, st.KeyFrames, st.Bytes, st.IsHardware, st.EncodeUs)

	if runErr != nil && !errors.Is(runErr, context.DeadlineExceeded) && !errors.Is(runErr, context.Canceled) {
		fmt.Fprintln(os.Stderr, "pipeline failed:", runErr)
		os.Exit(1)
	}
}
