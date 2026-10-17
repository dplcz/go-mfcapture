package capture

import (
	"context"
	"fmt"
	"log/slog"
	"structs"
	"sync/atomic"
	"syscall"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"
	"golang.org/x/sys/windows"
)

var nextCaptureID atomic.Uint64

// Diagnostic state is only accessed on the capture thread. It never determines
// whether a frame is released or a duplication session is recreated.
type duplicationDiagnostics struct {
	id                    uint64
	ownerThread           uint32
	generation            uint64
	acquireSeq            uint64
	lastReleaseSeq        uint64
	lastReleaseGeneration uint64
	lastReleaseHR         d3d.HRESULT
}

func newDuplicationDiagnostics() duplicationDiagnostics {
	return duplicationDiagnostics{
		id:          nextCaptureID.Add(1),
		ownerThread: windows.GetCurrentThreadId(),
	}
}

func (d *Duplicator) logDXGI(level slog.Level, operation string, hr d3d.HRESULT, extra ...slog.Attr) {
	logger := slog.Default()
	ctx := context.Background()
	if !logger.Enabled(ctx, level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("operation", operation),
		slog.Uint64("capture_id", d.diag.id),
		slog.Uint64("generation", d.diag.generation),
		slog.String("duplication", fmt.Sprintf("%p", d.outputDuplication)),
		slog.String("device", fmt.Sprintf("%p", d.device)),
		slog.Uint64("output", uint64(d.outputIndex)),
		slog.Uint64("thread_id", uint64(windows.GetCurrentThreadId())),
		slog.Uint64("owner_thread_id", uint64(d.diag.ownerThread)),
		slog.Uint64("acquire_seq", d.diag.acquireSeq),
		slog.Bool("local_owns_frame", d.acquiredFrame),
		slog.String("hr", fmt.Sprintf("0x%08x", uint32(hr))),
	}
	if d.diag.lastReleaseSeq != 0 {
		attrs = append(attrs,
			slog.Uint64("last_release_seq", d.diag.lastReleaseSeq),
			slog.Uint64("last_release_generation", d.diag.lastReleaseGeneration),
			slog.String("last_release_hr", fmt.Sprintf("0x%08x", uint32(d.diag.lastReleaseHR))),
		)
	}
	if hr.Failed() && hr != d3d.DXGI_ERROR_WAIT_TIMEOUT && d.device != nil {
		reason := deviceRemovedReason(d.device)
		attrs = append(attrs, slog.String("device_removed_hr", fmt.Sprintf("0x%08x", uint32(reason))))
	}
	attrs = append(attrs, extra...)
	logger.LogAttrs(ctx, level, "DXGI capture", attrs...)
}

// go-d3d exposes the device vtable but has no GetDeviceRemovedReason wrapper.
// Match its COM object layout and use the named vtable entry, without a hardcoded
// slot number. This query is diagnostic only; it does not reset the device.
type diagnosticDevice struct {
	_    structs.HostLayout
	vtbl *d3d11.ID3D11DeviceVtbl
}

func deviceRemovedReason(device *d3d11.ID3D11Device) d3d.HRESULT {
	obj := (*diagnosticDevice)(unsafe.Pointer(device))
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetDeviceRemovedReason, uintptr(unsafe.Pointer(device)))
	return d3d.HRESULT(uint32(ret))
}
