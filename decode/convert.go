package decode

import (
	"fmt"

	"mfcapture/mf"
)

// ColorMatrix 指定 YUV -> RGB 使用的转换矩阵。
type ColorMatrix int

const (
	// ColorMatrixAuto 按分辨率选：>=1280 宽用 BT.709（HD），否则 BT.601（SD）。
	// 与大多数播放器的默认行为一致。
	ColorMatrixAuto ColorMatrix = iota
	ColorMatrixBT601
	ColorMatrixBT709
)

// ToBGRA 把解码帧转成 BGRA 像素（自上而下，每像素 4 字节，A 恒为 255），
// 供需要 RGB 的渲染器 / UI 工具箱使用。
//
// 目前支持 NV12（硬件与软件解码器的默认输出）。如果你的渲染器能直接吃 NV12
// （D3D11 视频处理器、或者 ffmpeg 的 -pix_fmt nv12），就别走这一步——
// 多一次转换就是多一次全画幅拷贝。
func ToBGRA(f *DecodedFrame, m ColorMatrix) ([]byte, error) {
	if f == nil {
		return nil, fmt.Errorf("decode: nil frame")
	}
	switch f.Subtype {
	case mf.MFVideoFormat_NV12:
		return nv12ToBGRA(f, m)
	default:
		return nil, fmt.Errorf("decode: 暂不支持从 %s 转 BGRA（只实现了 NV12）", mf.SubtypeName(f.Subtype))
	}
}

func nv12ToBGRA(f *DecodedFrame, m ColorMatrix) ([]byte, error) {
	w, h := int(f.Width), int(f.Height)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("decode: 帧尺寸无效 %dx%d", f.Width, f.Height)
	}
	stride := int(f.Stride)
	if stride <= 0 {
		stride = w
	}
	if stride < w {
		return nil, fmt.Errorf("decode: stride %d 小于宽度 %d", stride, w)
	}
	need := stride*h + stride*(h/2)
	if len(f.Data) < need {
		return nil, fmt.Errorf("decode: 数据长度 %d 不足（%s %dx%d stride=%d 需要 %d）",
			len(f.Data), mf.SubtypeName(f.Subtype), w, h, stride, need)
	}

	yPlane := f.Data[:stride*h]
	uvPlane := f.Data[stride*h:]

	// 色度转换系数（limited range / TV range: Y 16..235, CbCr 16..240）
	cr, cb, cg, cg2 := 0, 0, 0, 0
	use709 := m == ColorMatrixBT709 || (m == ColorMatrixAuto && w >= 1280)
	if use709 {
		cr, cb, cg, cg2 = 459, 541, 54, 137 // 1.793, 2.112, 0.213, 0.533 (x256)
	} else {
		cr, cb, cg, cg2 = 408, 516, 100, 208 // 1.596, 2.017, 0.392, 0.813 (x256)
	}

	out := make([]byte, w*h*4)
	for row := 0; row < h; row++ {
		yRow := yPlane[row*stride:]
		uvRow := uvPlane[(row/2)*stride:]
		o := row * w * 4
		for col := 0; col < w; col++ {
			y := int(yRow[col]) - 16
			u := int(uvRow[col&^1]) - 128
			v := int(uvRow[col|1]) - 128

			yy := 298 * y // 1.164 (x256)
			r := (yy + cr*v) >> 8
			g := (yy - cg*u - cg2*v) >> 8
			b := (yy + cb*u) >> 8

			if r < 0 {
				r = 0
			} else if r > 255 {
				r = 255
			}
			if g < 0 {
				g = 0
			} else if g > 255 {
				g = 255
			}
			if b < 0 {
				b = 0
			} else if b > 255 {
				b = 255
			}

			out[o] = byte(b)
			out[o+1] = byte(g)
			out[o+2] = byte(r)
			out[o+3] = 255
			o += 4
		}
	}
	return out, nil
}

// NV12FrameSize 返回给定宽高（按 stride 对齐后）的 NV12 帧字节数，便于预分配。
func NV12FrameSize(width, height, stride int32) int {
	if stride <= 0 {
		stride = width
	}
	return int(stride)*int(height) + int(stride)*(int(height)/2)
}
