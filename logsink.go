package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logSink 是 log 包的自定义输出：按行切分后存入 hub 环形缓冲，
// 并实时转发给已打开日志窗口（log-subscribe）的浏览器连接。
// hub 绑定前（启动早期）的行暂存 pending，绑定后灌入缓冲（启动失败/错误日志不丢）。
type logSink struct {
	mu      sync.Mutex
	buf     []byte
	pending []string // 启动早期（hub 未创建）的行
	h       *hub
}

var logSinkInst = &logSink{}

// logChan 实时转发队列：sink.Write 只做行切分并非阻塞投递（日志输出可能在 h.mu 等锁内发生，
// 不能同步取 h.mu 广播——否则锁内日志造成死锁）；独立 goroutine 消费后广播。
// 队列满丢弃（文件日志保证完整；缓冲 256 通常足够）
var (
	logChan     = make(chan string, 256)
	logChanOnce sync.Once
)

// Write 实现 io.Writer；log 包内部串行调用，行切分按 \n 处理
func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := string(s.buf[:i+1])
		s.buf = s.buf[i+1:]
		if s.h != nil {
			select {
			case logChan <- line:
			default: // 队列满：丢弃实时转发（文件日志已完整）
			}
		} else {
			s.pending = append(s.pending, line)
			if len(s.pending) > logBufMax {
				s.pending = s.pending[len(s.pending)-logBufMax:]
			}
		}
	}
	return len(p), nil
}

// attach 绑定 hub（newHub 后调用）：灌入启动早期日志，启动广播 goroutine
func (s *logSink) attach(h *hub) {
	s.mu.Lock()
	s.h = h
	pend := append([]string(nil), s.pending...)
	s.pending = nil
	s.mu.Unlock()
	if len(pend) > 0 {
		h.logBufMu.Lock()
		h.logBuf = append(h.logBuf, pend...)
		if len(h.logBuf) > logBufMax {
			h.logBuf = h.logBuf[len(h.logBuf)-logBufMax:]
		}
		h.logBufMu.Unlock()
	}
	logChanOnce.Do(func() {
		go func() {
			for line := range logChan {
				h.pushLog(line)
			}
		}()
	})
}

const logBufMax = 500

// pushLog 存入环形缓冲并转发给已订阅的连接
func (h *hub) pushLog(line string) {
	h.logBufMu.Lock()
	h.logBuf = append(h.logBuf, line)
	if len(h.logBuf) > logBufMax {
		h.logBuf = h.logBuf[len(h.logBuf)-logBufMax:]
	}
	subs := make([]string, 0, len(h.logSubs))
	for id := range h.logSubs {
		subs = append(subs, id)
	}
	h.logBufMu.Unlock()

	h.mu.Lock()
	for _, id := range subs {
		if p := h.peers[id]; p != nil {
			sendMsg(p, map[string]any{"type": "log", "line": line})
		}
	}
	h.mu.Unlock()
}

// logSubscribe 打开日志窗口：标记订阅并回发缓冲历史
func (h *hub) logSubscribe(p *peer) {
	h.logBufMu.Lock()
	h.logSubs[p.id] = true
	lines := append([]string(nil), h.logBuf...)
	h.logBufMu.Unlock()
	sendMsg(p, map[string]any{"type": "log-history", "lines": lines})
}

// logUnsubscribe 关闭日志窗口
func (h *hub) logUnsubscribe(p *peer) {
	h.logBufMu.Lock()
	delete(h.logSubs, p.id)
	h.logBufMu.Unlock()
}

// logUnsubscribeID 断开清理
func (h *hub) logUnsubscribeID(id string) {
	h.logBufMu.Lock()
	delete(h.logSubs, id)
	h.logBufMu.Unlock()
}

// initLogFile 打开日志文件（logs/teartcshare-YYYYMMDD-HHMMSS.log，按启动时刻命名，追加）；
// 失败不中断启动（日志仅输出控制台），返回 nil
func initLogFile() *os.File {
	if err := os.MkdirAll("logs", 0o755); err != nil {
		log.Printf("创建日志目录失败（日志仅输出控制台）: %v", err)
		return nil
	}
	path := filepath.Join("logs", "teartcshare-"+time.Now().Format("20060102-150405")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("日志文件打开失败（日志仅输出控制台）: %v", err)
		return nil
	}
	return f
}

// syncWriter 包装 *os.File：每次写入后落盘（进程异常退出也不丢日志）
type syncWriter struct {
	f *os.File
}

func (w syncWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		return n, err
	}
	_ = w.f.Sync()
	return n, nil
}

// ignoreErrWriter 包装：写入失败不返回错误（不阻断 io.MultiWriter 后续 writer）。
// 静默启动 FreeConsole 后 os.Stderr 无效——若直接放 MultiWriter 里，写失败会提前返回，
// 导致文件日志与 WS 日志窗口都不再输出；此包装保证日志照常走文件 + 前端日志窗口
type ignoreErrWriter struct {
	w io.Writer
}

func (w ignoreErrWriter) Write(p []byte) (int, error) {
	_, _ = w.w.Write(p)
	return len(p), nil
}
