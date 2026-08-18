//go:build windows

package main

import (
	"syscall"
)

// 静默启动：启动时 FreeConsole 分离控制台（无黑窗后台运行；日志走文件 + 前端日志窗口）。
// 不拦截控制台关闭（X 直接退出进程，系统默认行为），也不操作 user32 窗口 API。

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole = kernel32.NewProc("FreeConsole")
)

// freeConsole 分离控制台窗口（进程继续运行；日志已走文件 + 前端日志窗口，不依赖控制台）
func freeConsole() {
	procFreeConsole.Call() //nolint:errcheck
}
