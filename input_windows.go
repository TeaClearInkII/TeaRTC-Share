//go:build windows

package main

import (
	"sync"
	"syscall"
	"unsafe"
)

// 远程控制输入执行层（Windows）：user32.SendInput 注入鼠标/键盘。
// 所有输入经 ctrlQueue 异步队列串行执行（保最新：队列满时丢弃最旧，保证拖动/移动流畅）。
// 坐标约定：鼠标使用 MOUSEEVENTF_ABSOLUTE 归一化坐标（0-65535，相对主屏虚拟桌面）。

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procSendInput    = user32.NewProc("SendInput")
	procSetCursorPos = user32.NewProc("SetCursorPos")

	ctrlQueue     = make(chan func(), 512)
	ctrlQueueOnce sync.Once
)

func initCtrlQueue() {
	ctrlQueueOnce.Do(func() {
		go func() {
			for f := range ctrlQueue {
				f()
			}
		}()
	})
}

// queueInput 串行入队；队列满时丢弃最旧事件（保最新状态，拖动跟手）
func queueInput(f func()) {
	initCtrlQueue()
	select {
	case ctrlQueue <- f:
		return
	default:
	}
	// 满：清空积压（保最新）
	for {
		select {
		case <-ctrlQueue:
		default:
			goto push
		}
	}
push:
	select {
	case ctrlQueue <- f:
	default:
	}
}

// ---- SendInput 原始调用 ----

// INPUT 布局（64 位 Windows）：type(DWORD,0) + padding(4) + union(8 起)
// union 按 MOUSEINPUT(32B) 布局，sizeof(INPUT)=40
func sendRaw(b []byte) {
	if len(b) == 0 {
		return
	}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b))) //nolint:errcheck
}

func put16(b []byte, off int, v uint16) { b[off] = byte(v); b[off+1] = byte(v >> 8) }
func put32(b []byte, off int, v int32)  { put16(b, off, uint16(v)); put16(b, off+2, uint16(v>>16)) }

func mouseInput(dx, dy, mouseData, flags int32) {
	b := make([]byte, 40)
	put32(b, 0, 0) // INPUT_MOUSE
	put32(b, 8, dx)
	put32(b, 12, dy)
	put32(b, 16, mouseData)
	put32(b, 20, flags)
	sendRaw(b)
}

func keyInput(vk uint16, flags uint32) {
	b := make([]byte, 40)
	put32(b, 0, 1) // INPUT_KEYBOARD
	put16(b, 8, vk)
	put16(b, 10, 0) // wScan
	put32(b, 12, int32(flags))
	sendRaw(b)
}

// ---- 鼠标事件常量 ----
const (
	mouseMoveAbs    = 0x0001 | 0x8000 // MOVE | ABSOLUTE
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseWheel      = 0x0800
	keyUp           = 0x0002
	keyExt          = 0x0100
	maxAbs          = 65535
	maxWheelDelta   = 32767
)

// ---- 对外控制接口（异步队列串行执行） ----

// ctrlMouseMove 移动鼠标到虚拟桌面物理像素坐标（可负、可超出主屏）。
// 用 SetCursorPos（跨屏正确；SendInput 的 MOUSEEVENTF_ABSOLUTE 只映射主屏，副屏到不了）
func ctrlMouseMove(x, y int) {
	queueInput(func() { procSetCursorPos.Call(uintptr(x), uintptr(y)) }) //nolint:errcheck
}

// ctrlMouseButton 鼠标按键（down=true 按下 / false 抬起）
func ctrlMouseButton(button string, down bool) {
	var d, u int32
	switch button {
	case "right":
		d, u = mouseRightDown, mouseRightUp
	case "middle":
		d, u = mouseMiddleDown, mouseMiddleUp
	default:
		d, u = mouseLeftDown, mouseLeftUp
	}
	queueInput(func() {
		if down {
			mouseInput(0, 0, 0, d)
		} else {
			mouseInput(0, 0, 0, u)
		}
	})
}

// ctrlMouseWheel 滚轮（dy 为滚动行数，正=上滚）
func ctrlMouseWheel(dy int) {
	if dy > 300 {
		dy = 300
	}
	if dy < -300 {
		dy = -300
	}
	queueInput(func() { mouseInput(0, 0, int32(dy*120), mouseWheel) })
}

// ctrlKey 按键（code 为 event.code 物理键；down=true 按下 / false 抬起）
func ctrlKey(code string, down bool) {
	vk, ext, ok := vkForCode(code)
	if !ok {
		return
	}
	queueInput(func() {
		flags := uint32(0)
		if ext {
			flags |= keyExt
		}
		if !down {
			flags |= keyUp
		}
		keyInput(vk, flags)
	})
}

// ---- event.code → Windows VK 映射（标准 US 布局；ext 标注需要 KEYEVENTF_EXTENDEDKEY 的键） ----

type vkEntry struct {
	vk  uint16
	ext bool
}

var keyCodeMap = func() map[string]vkEntry {
	m := map[string]vkEntry{}
	for i := 0; i < 26; i++ {
		m["Key"+string(rune('A'+i))] = vkEntry{0x41 + uint16(i), false}
	}
	for i := 0; i < 10; i++ {
		m["Digit"+string(rune('0'+i))] = vkEntry{0x30 + uint16(i), false}
		m["Numpad"+string(rune('0'+i))] = vkEntry{0x60 + uint16(i), false}
	}
	for i := 1; i <= 24; i++ {
		m["F"+itoa(i)] = vkEntry{0x70 + uint16(i-1), false}
	}
	m["ArrowLeft"] = vkEntry{0x25, true}
	m["ArrowUp"] = vkEntry{0x26, true}
	m["ArrowRight"] = vkEntry{0x27, true}
	m["ArrowDown"] = vkEntry{0x28, true}
	m["ShiftLeft"] = vkEntry{0xA0, false}
	m["ShiftRight"] = vkEntry{0xA1, false}
	m["ControlLeft"] = vkEntry{0xA2, false}
	m["ControlRight"] = vkEntry{0xA3, true}
	m["AltLeft"] = vkEntry{0xA4, false}
	m["AltRight"] = vkEntry{0xA5, true}
	m["MetaLeft"] = vkEntry{0x5B, false}
	m["MetaRight"] = vkEntry{0x5C, true}
	m["Enter"] = vkEntry{0x0D, false}
	m["NumpadEnter"] = vkEntry{0x0D, true}
	m["Backspace"] = vkEntry{0x08, false}
	m["Tab"] = vkEntry{0x09, false}
	m["Space"] = vkEntry{0x20, false}
	m["Escape"] = vkEntry{0x1B, false}
	m["Home"] = vkEntry{0x24, true}
	m["End"] = vkEntry{0x23, true}
	m["PageUp"] = vkEntry{0x21, true}
	m["PageDown"] = vkEntry{0x22, true}
	m["Insert"] = vkEntry{0x2D, true}
	m["Delete"] = vkEntry{0x2E, true}
	m["CapsLock"] = vkEntry{0x14, false}
	m["NumLock"] = vkEntry{0x90, false}
	m["ScrollLock"] = vkEntry{0x91, false}
	m["Semicolon"] = vkEntry{0xBA, false}
	m["Equal"] = vkEntry{0xBB, false}
	m["Comma"] = vkEntry{0xBC, false}
	m["Minus"] = vkEntry{0xBD, false}
	m["Period"] = vkEntry{0xBE, false}
	m["Slash"] = vkEntry{0xBF, false}
	m["Backquote"] = vkEntry{0xC0, false}
	m["BracketLeft"] = vkEntry{0xDB, false}
	m["Backslash"] = vkEntry{0xDC, false}
	m["BracketRight"] = vkEntry{0xDD, false}
	m["Quote"] = vkEntry{0xDE, false}
	return m
}()

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func vkForCode(code string) (uint16, bool, bool) {
	e, ok := keyCodeMap[code]
	if !ok {
		return 0, false, false
	}
	return e.vk, e.ext, true
}
