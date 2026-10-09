package capture

import (
	"github.com/kirides/go-d3d/d3d11"

	"mfcapture/gpu"
)

// NewVideoCapableD3D11Device 创建带 VIDEO_SUPPORT|BGRA_SUPPORT 标志的 D3D11 设备。
// 实现已移到 gpu 包（解码器要用同一个设备来做 DXVA 硬件解码），这里保留转发，
// capture 的对外 API 不变。
func NewVideoCapableD3D11Device() (*d3d11.ID3D11Device, *d3d11.ID3D11DeviceContext, error) {
	return gpu.NewVideoCapableDevice()
}

// EnableMultithreadProtection 见 gpu.EnableMultithreadProtection。
func EnableMultithreadProtection(device *d3d11.ID3D11Device) error {
	return gpu.EnableMultithreadProtection(device)
}
