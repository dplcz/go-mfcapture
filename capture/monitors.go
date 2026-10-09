package capture

import (
	"fmt"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/dxgi"
	"golang.org/x/sys/windows"
)

// MonitorInfo 描述一个可被采集的显示输出。
// Index 与 mfcapture.PipelineConfig.Output 使用同一套编号，可直接传给采集管线。
type MonitorInfo struct {
	Index    uint
	Name     string // 形如 \\.\DISPLAY1
	Width    uint32
	Height   uint32
	Left     int32
	Top      int32
	Primary  bool // 桌面坐标原点在该输出上
	Attached bool // 当前已挂载到桌面
}

// EnumMonitors 枚举可用于桌面复制的显示输出。
//
// 只做 DXGI 查询，不需要 COM/MF 初始化，可在任意 goroutine 调用。
//
// 编号语义刻意与 NewVideoCapableD3D11Device + NewDuplicator 保持一致：只枚举
// D3D 设备实际所在的那块适配器（第一块非软件适配器）上的输出。因此返回的 Index
// 可以直接赋给 PipelineConfig.Output。多显卡机器上挂在其它适配器上的显示器不会出现在
// 这里——那也是采集管线当前拿不到的输出，保持一致比"列出来但用不了"更有用。
func EnumMonitors() ([]MonitorInfo, error) {
	var factory1 *dxgi.IDXGIFactory1
	if err := dxgi.CreateDXGIFactory1(&factory1); err != nil {
		return nil, fmt.Errorf("CreateDXGIFactory1: %w", err)
	}
	defer factory1.Release()

	adapter, err := firstHardwareAdapter(factory1)
	if err != nil {
		return nil, err
	}
	defer adapter.Release()

	var out []MonitorInfo
	for i := uint32(0); ; i++ {
		var output *dxgi.IDXGIOutput
		hr := adapter.EnumOutputs(i, &output)
		if d3d.HRESULT(hr).Failed() {
			break
		}
		info, ok := describeOutput(output)
		output.Release()
		if !ok {
			continue
		}
		info.Index = uint(len(out))
		out = append(out, info)
	}
	return out, nil
}

// firstHardwareAdapter 选出第一块非软件适配器，逻辑与 NewVideoCapableD3D11Device 一致，
// 保证 MonitorInfo.Index 能被 NewDuplicator 正确解释。
func firstHardwareAdapter(factory1 *dxgi.IDXGIFactory1) (*dxgi.IDXGIAdapter1, error) {
	var desc dxgi.DXGI_ADAPTER_DESC1

	for i := uint32(0); ; i++ {
		var a *dxgi.IDXGIAdapter1
		hr := factory1.EnumAdapters1(i, &a)
		if d3d.HRESULT(hr).Failed() {
			break
		}
		if d3d.HRESULT(a.GetDesc1(&desc)).Failed() ||
			(desc.Flags&dxgi.DXGI_ADAPTER_FLAG_SOFTWARE) != 0 {
			a.Release()
			continue
		}
		return a, nil
	}

	// 没有任何非软件适配器（纯软件渲染环境）：退回第一块，与 NewVideoCapableD3D11Device 的兜底一致。
	var a *dxgi.IDXGIAdapter1
	if hr := factory1.EnumAdapters1(0, &a); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("EnumAdapters1: %w", d3d.HRESULT(hr))
	}
	return a, nil
}

func describeOutput(output *dxgi.IDXGIOutput) (MonitorInfo, bool) {
	// go-d3d 只给 IDXGIOutput5 绑了 GetDesc，先 QI 上去。
	var out5 *dxgi.IDXGIOutput5
	if hr := output.QueryInterface(dxgi.IID_IDXGIOutput5, &out5); d3d.HRESULT(hr).Failed() {
		return MonitorInfo{}, false
	}
	defer out5.Release()

	var desc dxgi.DXGI_OUTPUT_DESC
	if hr := out5.GetDesc(&desc); d3d.HRESULT(hr).Failed() {
		return MonitorInfo{}, false
	}

	r := desc.DesktopCoordinates
	return MonitorInfo{
		Name:     windows.UTF16ToString(desc.DeviceName[:]),
		Width:    uint32(r.Right - r.Left),
		Height:   uint32(r.Bottom - r.Top),
		Left:     r.Left,
		Top:      r.Top,
		Primary:  r.Left == 0 && r.Top == 0,
		Attached: desc.AttachedToDesktop != 0,
	}, true
}
