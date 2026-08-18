//go:build windows

package main

import (
	"sync"
	"syscall"
	"unsafe"
)

// 多显示器布局检测：远程控制坐标换算依据（把"某屏幕内相对坐标"映射到"虚拟桌面绝对坐标"）。

type monitorInfo struct {
	Left    int
	Top     int
	Width   int
	Height  int
	Primary bool
}

type monitorLayout struct {
	Monitors []monitorInfo
	MinX     int // 虚拟桌面包围盒（逻辑像素，可能为负：副屏在主屏左侧/上方）
	MinY     int
	MaxX     int
	MaxY     int
}

var (
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfo      = user32.NewProc("GetMonitorInfoW")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")

	monitorOnce  sync.Once
	monitorCache monitorLayout
)

func init() {
	// DPI-aware：GetMonitorInfo/SendInput 均用物理像素，与浏览器 getDisplayMedia 的物理分辨率一致
	procSetProcessDPIAware.Call() //nolint:errcheck
}

// currentMonitorLayout 缓存后的屏幕布局（首次调用时枚举）
func currentMonitorLayout() monitorLayout {
	monitorOnce.Do(func() { monitorCache = monitorLayouts() })
	return monitorCache
}

const monitorInfoFPrimary = 0x1

type rectT struct {
	Left, Top, Right, Bottom int32
}

type monitorInfoW struct {
	cbSize    uint32
	rcMonitor rectT
	rcWork    rectT
	dwFlags   uint32
}

// monitorLayouts 枚举全部显示器布局（逻辑坐标）与虚拟桌面包围盒
func monitorLayouts() monitorLayout {
	var layout monitorLayout
	procEnumDisplayMonitors.Call(0, 0,
		syscall.NewCallback(func(hmon, hdc uintptr, rect *rectT, dw uintptr) uintptr {
			var mi monitorInfoW
			mi.cbSize = uint32(unsafe.Sizeof(mi))
			if r, _, _ := procGetMonitorInfo.Call(hmon, uintptr(unsafe.Pointer(&mi))); r != 0 {
				layout.Monitors = append(layout.Monitors, monitorInfo{
					Left:    int(mi.rcMonitor.Left),
					Top:     int(mi.rcMonitor.Top),
					Width:   int(mi.rcMonitor.Right - mi.rcMonitor.Left),
					Height:  int(mi.rcMonitor.Bottom - mi.rcMonitor.Top),
					Primary: mi.dwFlags&monitorInfoFPrimary != 0,
				})
			}
			return 1
		}), 0)
	if len(layout.Monitors) > 0 {
		layout.MinX, layout.MinY = layout.Monitors[0].Left, layout.Monitors[0].Top
		layout.MaxX = layout.Monitors[0].Left + layout.Monitors[0].Width
		layout.MaxY = layout.Monitors[0].Top + layout.Monitors[0].Height
		for _, m := range layout.Monitors {
			r, b := m.Left+m.Width, m.Top+m.Height
			if m.Left < layout.MinX {
				layout.MinX = m.Left
			}
			if m.Top < layout.MinY {
				layout.MinY = m.Top
			}
			if r > layout.MaxX {
				layout.MaxX = r
			}
			if b > layout.MaxY {
				layout.MaxY = b
			}
		}
	}
	return layout
}

// matchMonitor 按分辨率匹配屏幕（主屏优先；同分辨率多屏时主屏更可能是共享目标）
func (l monitorLayout) matchMonitor(width, height int) (monitorInfo, bool) {
	for _, m := range l.Monitors {
		if m.Primary && m.Width == width && m.Height == height {
			return m, true
		}
	}
	for _, m := range l.Monitors {
		if m.Width == width && m.Height == height {
			return m, true
		}
	}
	return monitorInfo{}, false
}

// primaryMonitor 返回主屏（无主屏时取第一块）
func (l monitorLayout) primaryMonitor() monitorInfo {
	for _, m := range l.Monitors {
		if m.Primary {
			return m
		}
	}
	if len(l.Monitors) > 0 {
		return l.Monitors[0]
	}
	return monitorInfo{}
}

// toVirtualPx 把"某屏幕内相对坐标(0-65535)"换算为虚拟桌面物理像素坐标（可负，跨屏）
func (l monitorLayout) toVirtualPx(m monitorInfo, relX, relY int) (int, int) {
	if m.Width <= 0 || m.Height <= 0 {
		return 0, 0
	}
	px := m.Left + int(float64(relX)/65535*float64(m.Width))
	py := m.Top + int(float64(relY)/65535*float64(m.Height))
	return px, py
}
