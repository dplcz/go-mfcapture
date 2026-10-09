// Command mfdecode 把一个 Annex-B 的 .h264 文件解码成原始像素文件。
//
// 用途：验证解码链路（mfencode 产出的码流 -> 解码器 -> 像素），也是将来
// 客户端渲染的原型。输出默认是 NV12 原始帧。
//
//	go run ./cmd/mfencode -duration 3s -o out.h264
//	go run ./cmd/mfdecode -i out.h264 -o out.nv12
//	ffplay -f rawvideo -pix_fmt nv12 -video_size 2560x1440 out.nv12
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	d3d "github.com/kirides/go-d3d"

	"mfcapture/decode"
	"mfcapture/mf"
)

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

	runtime.LockOSThread()
	if hr := mf.CoInitializeEx(); d3d.HRESULT(hr).Failed() {
		fmt.Fprintln(os.Stderr, "CoInitializeEx:", d3d.HRESULT(hr))
		os.Exit(1)
	}
	defer mf.CoUninitialize()
	if hr := mf.MFStartup(); d3d.HRESULT(hr).Failed() {
		fmt.Fprintln(os.Stderr, "MFStartup:", d3d.HRESULT(hr))
		os.Exit(1)
	}
	defer mf.MFShutdown()

	dec, err := decode.NewH264MFTDecoder(decode.H264DecoderConfig{
		Width:           uint32(*w),
		Height:          uint32(*h),
		FPSNum:          30,
		FPSDen:          1,
		DisableHardware: *nohw,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "NewH264MFTDecoder:", err)
		os.Exit(1)
	}
	defer dec.Release()

	sub, ow, oh, stride := dec.OutputFormat()
	fmt.Printf("mfdecode: decoder DXVA(硬件加速)=%v output=%s %dx%d stride=%d\n",
		dec.IsHardware, mf.SubtypeName(sub), ow, oh, stride)

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create output:", err)
		os.Exit(1)
	}
	defer f.Close()

	var frames, bytes int64
	start := time.Now()
	for i, au := range aus {
		ts := time.Duration(i) * time.Second / 30
		got, err := dec.Decode(au, ts, time.Second/30)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mfdecode: AU %d: %v\n", i, err)
			continue
		}
		for _, fr := range got {
			frames++
			bytes += write(f, fr, *bgra)
		}
	}
	tail, _ := dec.Flush()
	for _, fr := range tail {
		frames++
		bytes += write(f, fr, *bgra)
	}

	elapsed := time.Since(start)
	fmt.Printf("mfdecode: done. %d frames, %d bytes in %s\n", frames, bytes, elapsed.Round(time.Millisecond))
	if frames == 0 {
		fmt.Fprintln(os.Stderr, "mfdecode: FAIL - no frames decoded")
		os.Exit(1)
	}
	fmt.Printf("mfdecode: OK. %d 帧，平均 %.2f ms/帧（%.0f fps）\n",
		frames, float64(elapsed.Milliseconds())/float64(frames), float64(frames)/elapsed.Seconds())
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
		pos := j + 3
		if pos >= len(data) {
			break
		}
		nals = append(nals, nal{start: j, typ: data[pos] & 0x1F})
		i = pos + 1
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

// indexStartCode 找 00 00 01（3 字节或 4 字节起始码的公共尾部）的位置。
func indexStartCode(b []byte, from int) int {
	for i := from; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i
		}
	}
	return -1
}
