// Command mfdiag 列出本机可用的 H264 编码器 MFT，以及每个编码器在给定分辨率下
// 真正接受的输入像素格式。
//
// 用途：当 NewH264MFTEncoder 报 SetInputType: 0xC00D36B4 (MF_E_INVALIDMEDIATYPE) 时，
// 用这个工具看清楚"到底哪个编码器被选中了、它能吃什么格式"。
//
//	go run ./cmd/mfdiag            # 用主显示器分辨率
//	go run ./cmd/mfdiag -w 1920 -h 1080
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	d3d "github.com/kirides/go-d3d"

	"mfcapture/capture"
	"mfcapture/encode"
	"mfcapture/mf"
)

type group struct {
	name  string
	flags uint32
}

func main() {
	w := flag.Uint("w", 0, "frame width (0 = primary monitor)")
	h := flag.Uint("h", 0, "frame height (0 = primary monitor)")
	bitrate := flag.Uint("bitrate", 8_000_000, "target bitrate for the probe")
	flag.Parse()

	if *w == 0 || *h == 0 {
		monitors, err := capture.EnumMonitors()
		if err != nil || len(monitors) == 0 {
			fmt.Fprintln(os.Stderr, "EnumMonitors:", err, "- 请用 -w/-h 指定分辨率")
			os.Exit(1)
		}
		m := monitors[0]
		fmt.Printf("主显示器: %s %dx%d\n\n", m.Name, m.Width, m.Height)
		*w, *h = uint(m.Width), uint(m.Height)
	}

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

	outType := mf.MFT_REGISTER_TYPE_INFO{
		GuidMajorType: mf.MFMediaType_Video,
		GuidSubtype:   mf.MFVideoFormat_H264,
	}

	groups := []group{
		{"硬件 (MFT_ENUM_FLAG_HARDWARE)", mf.MFT_ENUM_FLAG_HARDWARE | mf.MFT_ENUM_FLAG_SORTANDFILTER},
		{"软件/同步 (MFT_ENUM_FLAG_SYNCMFT)", mf.MFT_ENUM_FLAG_SYNCMFT | mf.MFT_ENUM_FLAG_SORTANDFILTER},
	}

	argbOK := false
	for _, g := range groups {
		acts, err := mf.MFTEnumEx(mf.MFT_CATEGORY_VIDEO_ENCODER, g.flags, nil, &outType)
		if err != nil {
			fmt.Printf("[%s] 枚举失败: %v\n", g.name, err)
			continue
		}
		fmt.Printf("[%s] 找到 %d 个\n", g.name, len(acts))
		for i, act := range acts {
			var t *mf.IMFTransform
			hr := act.ActivateObject(mf.IID_IMFTransform, unsafe.Pointer(&t))
			if d3d.HRESULT(hr).Failed() {
				fmt.Printf("  #%d 激活失败: %v\n", i, d3d.HRESULT(hr))
				continue
			}
			probe(t, i, uint32(*w), uint32(*h), uint32(*bitrate))
			if tryARGB32(t, uint32(*w), uint32(*h)) {
				argbOK = true
			}
			t.Release()
		}
	}

	fmt.Println()
	if argbOK {
		fmt.Println("结论: 至少有一个编码器接受 ARGB32 输入 —— mfcapture 可以直接用。")
	} else {
		fmt.Println("结论: 没有任何编码器接受 ARGB32 输入。")
		fmt.Println("  这说明本机没有可用的硬件 H264 编码器（虚拟机/无显卡驱动时很常见），")
		fmt.Println("  回退到的微软软件编码器只接受 NV12/I420/IYUV/YV12/YUY2。")
		fmt.Println("  mfcapture 目前从 DXGI 拿到的是 BGRA 纹理，必须先做 BGRA -> NV12 转换才能喂给软件编码器。")
	}
}

func probe(t *mf.IMFTransform, idx int, w, h, bitrate uint32) {
	var attrs *mf.IMFAttributes
	d3dAware := "unknown"
	if hr := t.GetAttributes(&attrs); !d3d.HRESULT(hr).Failed() && attrs != nil {
		var v uint32
		if !d3d.HRESULT(attrs.GetUINT32(mf.MF_SA_D3D11_AWARE, &v)).Failed() {
			d3dAware = fmt.Sprintf("%d", v)
		}
		// 异步 MFT 必须先解锁，否则后续 SetOutputType/SetInputType 一律失败。
		// 这一步要和 encode.H264MFTEncoder.configure 保持一致，否则会误报。
		attrs.SetUINT32(mf.MF_TRANSFORM_ASYNC_UNLOCK, 1)
		attrs.Release()
	}

	// 编码器必须先定输出类型，输入类型列表才完整。
	outType, err := mf.NewOutputMediaType(w, h, 30, 1, bitrate)
	if err != nil {
		fmt.Printf("  #%d 构造输出类型失败: %v\n", idx, err)
		return
	}
	defer outType.Release()
	if hr := t.SetOutputType(0, outType, 0); d3d.HRESULT(hr).Failed() {
		fmt.Printf("  #%d SetOutputType(H264 %dx%d) 失败: %v\n", idx, w, h, d3d.HRESULT(hr))
		return
	}

	fmt.Printf("  #%d D3D11-aware=%s  接受的输入类型: %v\n", idx, d3dAware, encode.AvailableInputTypes(t))
}

func tryARGB32(t *mf.IMFTransform, w, h uint32) bool {
	inType, err := mf.NewInputMediaType(w, h, 30, 1)
	if err != nil {
		return false
	}
	defer inType.Release()
	return !d3d.HRESULT(t.SetInputType(0, inType, 0)).Failed()
}
