//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/getlantern/systray"
)

// appConfig 托盘/启动设置（config.json，工作目录）
type appConfig struct {
	SilentStart bool `json:"silentStart"` // 静默启动：启动不显示控制台窗口（后台运行）
}

var cfg = loadConfig()

func configPath() string {
	return filepath.Join(".", "config.json")
}

func loadConfig() appConfig {
	c := appConfig{SilentStart: false}
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

func saveConfig() {
	if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		_ = os.WriteFile(configPath(), b, 0o644)
	}
}

// runTray 启动系统托盘（阻塞，main goroutine 调用）：
// 菜单：打开页面 / 静默启动 / 退出。控制台 X 关闭 = 直接退出进程（系统默认）。
// 托盘「退出」→ systray.Quit → 本函数返回 → main 结束（进程退出）
func runTray(port int) {
	systray.Run(func() {
		systray.SetIcon(trayIcon) // Windows 托盘要求 .ico 格式（PNG 无效）
		systray.SetTitle("TeaRTC 屏幕共享")
		systray.SetTooltip("TeaRTC 屏幕共享 - 服务运行中")
		mPage := systray.AddMenuItem("打开页面", "浏览器打开本机观看地址")
		systray.AddSeparator()
		mSilent := systray.AddMenuItemCheckbox("静默启动", "下次启动不显示控制台窗口（后台运行）", cfg.SilentStart)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "退出程序")

		url := fmt.Sprintf("https://localhost:%d", port)
		for {
			select {
			case <-mPage.ClickedCh:
				openBrowser(url)
			case <-mSilent.ClickedCh:
				cfg.SilentStart = !cfg.SilentStart
				if cfg.SilentStart {
					mSilent.Check()
				} else {
					mSilent.Uncheck()
				}
				saveConfig()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}, func() {
		log.Println("tray: 已退出")
	})
}

// openBrowser 用默认浏览器打开 URL（Windows cmd start，隐藏控制台窗口）
func openBrowser(url string) {
	c := exec.Command("cmd", "/c", "start", "", url)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = c.Start()
}
