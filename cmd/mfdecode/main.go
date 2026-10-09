// Command mfdecode 把一个 Annex-B 的 .h264 文件解码成原始像素文件。
//
// 用途：验证解码链路（mfencode 产出的码流 -> 解码器 -> 像素），也是将来
// 客户端渲染的原型。输出默认是 NV12 原始帧。
//
//	go run ./cmd/mfencode -duration 3s -o out.h264
//	go run ./cmd/mfdecode -i out.h264 -o out.nv12
//	ffplay -f rawvideo -pix_fmt nv12 -video_size 2560x1440 out.nv12
//
// 本命令也是 decode.Session 的用例示范：调用方不碰 runtime.LockOSThread、
// 不碰 CoInitializeEx/MFStartup，全部由 Session.Run 自己收在内部。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"mfcapture/decode"
	"mfcapture/mf"
)

// fileSink 把解出的帧顺序写进输出文件，写完立刻归还。
type fileSink struct {
	f      *os.File
	bgra   bool
	frames int64
	bytes  int64
}

func (s *fileSink) OnFrame(fr *decode.DecodedFrame) {
	s.frames++
	s.bytes += write(s.f, fr, s.bgra)
	fr.Release() // GPU 路径下必须归还，否则解码器 surface 池会被占满
}

func (s *fileSink) OnEvent(e decode.Event) {
	switch e.Kind {
	case decode.EventReady:
		fmt.Printf("mfdecode: decoder DXVA(硬件加速)=%v output=%s %dx%d stride=%d\n",
			e.Format.Hardware, mf.SubtypeName(e.Format.Subtype),
			e.Format.Width, e.Format.Height, e.Format.Stride)
	case decode.EventFormatChanged:
		fmt.Printf("mfdecode: 输出格式变化 -> %s %dx%d stride=%d\n",
			mf.SubtypeName(e.Format.Subtype), e.Format.Width, e.Format.Height, e.Format.Stride)
	case decode.EventDegraded:
		fmt.Fprintf(os.Stderr, "mfdecode: DXVA 不可用，已回退到软件解码: %v\n", e.Err)
	case decode.EventDecodeError:
		fmt.Fprintf(os.Stderr, "mfdecode: 解码错误: %v\n", e.Err)
	}
}

func main() {
	in := flag.String("i", "out.h264", "input Annex-B H264 file")
	out := flag.String("o", "out.nv12", "output raw pixel file")
	w := flag.Uint("w", 0, "stream width (0 = let the decoder infer from SPS)")
	h := flag.Uint("h", 0, "stream height (0 = let the decoder infer from SPS)")
	verbose := flag.Bool("v", false, "打印解码器候选协商细节（排查为什么没用上硬件解码）")
	bgra := flag.Bool("bgra", false, "输出 BGRA 而不是 NV12（顺带验证 YUV->RGB 转换）")
	nohw := flag.Bool("nohw", false, "禁用 DXVA 硬件解码，用于对比耗时")
	flag.Parse()

	if *verbose {
		decode.DebugLog = func(s string) { fmt.Fprintln(os.Stderr, "  [diag]", s) }
		decode.FallbackLog = func(s string) { fmt.Fprintln(os.Stderr, "  [fallback]", s) }
	}

	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		os.Exit(1)
	}
	if len(data) == 0 {
		fmt.Fprintln(os.Stderr, "input file is empty")
		os.Exit(1)
	}

	aus := splitAccessUnits(data)
	fmt.Printf("mfdecode: %s -> %s, %d bytes, %d access units\n", *in, *out, len(data), len(aus))

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create output:", err)
		os.Exit(1)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Run 返回后放行喂数据的 goroutine

	sink := &fileSink{f: f, bgra: *bgra}
	sess, err := decode.NewSession(decode.SessionConfig{
		H264DecoderConfig: decode.H264DecoderConfig{
			Width:           uint32(*w),
			Height:          uint32(*h),
			FPSNum:          30,
			FPSDen:          1,
			DisableHardware: *nohw,
		},
	}, sink)
	if err != nil {
		fmt.Fprintln(os.Stderr, "NewSession:", err)
		os.Exit(1)
	}
	defer sess.Close()

	// 喂数据：Submit 只入队，不阻塞在解码上；队列满就稍等重试。
	// 全部投完声明输入结束，Run 会排空解码器后正常返回。
	go func() {
		for i, au := range aus {
			ts := time.Duration(i) * time.Second / 30
			for !sess.Submit(au, ts, time.Second/30) {
				select {
				case <-ctx.Done(): // Run 已结束，别再等了
					return
				case <-time.After(2 * time.Millisecond):
				}
			}
		}
		sess.EndOfStream()
	}()

	start := time.Now()
	// Run 内部完成 LockOSThread + COM/MF 初始化与清理，调用方什么都不用做。
	if err := sess.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "mfdecode: run:", err)
		os.Exit(1)
	}
	elapsed := time.Since(start)

	fmt.Printf("mfdecode: done. %d frames, %d bytes in %s\n", sink.frames, sink.bytes, elapsed.Round(time.Millisecond))
	if sink.frames == 0 {
		fmt.Fprintln(os.Stderr, "mfdecode: FAIL - no frames decoded")
		os.Exit(1)
	}
	fmt.Printf("mfdecode: OK. %d 帧，平均 %.2f ms/帧（%.0f fps）\n",
		sink.frames, float64(elapsed.Milliseconds())/float64(sink.frames), float64(sink.frames)/elapsed.Seconds())
}

func write(f *os.File, fr *decode.DecodedFrame, asBGRA bool) int64 {
	if !asBGRA {
		n, _ := f.Write(fr.Data)
		return int64(n)
	}
	bgra, err := decode.ToBGRA(fr, decode.ColorMatrixAuto)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ToBGRA:", err)
		os.Exit(1)
	}
	n, _ := f.Write(bgra)
	return int64(n)
}

// splitAccessUnits 把 Annex-B 字节流切成访问单元。
// 规则：当前 AU 已经有 slice 之后，再遇到 AUD/SPS/PPS/SEI/slice 就认为新 AU 开始。
func splitAccessUnits(data []byte) [][]byte {
	type nal struct {
		start int
		typ   byte
	}
	var nals []nal
	for i := 0; ; {
		j := indexStartCode(data, i)
		if j < 0 {
			break
		}
		hdrPos := j + 3
		if hdrPos >= len(data) {
			break
		}
		nalStart := j
		if j > 0 && data[j-1] == 0 {
			nalStart = j - 1
		}
		nals = append(nals, nal{start: nalStart, typ: data[hdrPos] & 0x1F})
		i = hdrPos + 1
	}
	if len(nals) == 0 {
		return nil
	}

	var aus [][]byte
	curStart := nals[0].start
	hasSlice := false
	for i, n := range nals {
		isSlice := n.typ == 1 || n.typ == 5
		isBoundary := n.typ == 9 || n.typ == 7 || n.typ == 8 || n.typ == 6
		if i > 0 && hasSlice && (isSlice || isBoundary) {
			aus = append(aus, data[curStart:n.start])
			curStart = n.start
			hasSlice = false
		}
		if isSlice {
			hasSlice = true
		}
	}
	aus = append(aus, data[curStart:])
	return aus
}

// indexStartCode 找 00 00 01（3 字节起始码的公共尾部）的位置。
func indexStartCode(b []byte, from int) int {
	for i := from; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i
		}
	}
	return -1
}
