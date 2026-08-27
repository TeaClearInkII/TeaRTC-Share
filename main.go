package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"
)

//go:embed web/index.html
var indexHTML embed.FS

//go:embed icon.png
var iconPNG []byte // 应用图标：exe（.syso 资源）+ favicon（/favicon.png）共用

//go:embed trayicon.ico
var trayIcon []byte // 托盘专用图标（Windows 托盘需 .ico 格式；裁剪放大+不透明底，16/32 双尺寸）

const (
	caOrg        = "TeaShare"
	caCN         = "TeaShare Local CA"
	caValidDays  = 3650
	srvValidDays = 365
	appVersion   = "0.4.1"
)

var verboseFlag atomic.Bool // 日志调试模式（-v 启动或运行时 /api/log-level 切换）

// logV 调试日志：仅调试模式输出（高频细节日志，生产默认安静）
func logV(format string, args ...any) {
	if verboseFlag.Load() {
		log.Printf(format, args...)
	}
}

func main() {
	port := flag.Int("port", 7553, "HTTPS 监听端口")
	turnPort := flag.Int("turnport", 3478, "TURN 中继 TCP 端口")
	verbose := flag.Bool("v", false, "调试日志（详细输出）")
	flag.Parse()
	verboseFlag.Store(*verbose)

	// 日志三路输出：控制台 + 文件（logs/teartcshare-YYYYMMDD.log，完整内容含启动失败/错误）+ WS 日志窗口。
	// 必须最先设置，确保启动早期（IPv6 探测/证书初始化等）的日志也落盘
	{
		outs := []io.Writer{ignoreErrWriter{w: os.Stderr}, logSinkInst} // stderr 写失败不阻断文件/WS 日志（静默模式 FreeConsole 后 stderr 无效）
		if lf := initLogFile(); lf != nil {
			outs = append(outs, syncWriter{f: lf})
		}
		log.SetOutput(io.MultiWriter(outs...))
	}
	if cfg.SilentStart {
		freeConsole() // 静默启动：分离控制台（无黑窗，日志走文件 + 前端日志窗口）
	}

	addrs := detectIPv6()
	ipv6Info.LocalAddrs = addrs // 启动期同步设置（单线程，无需锁）
	ipv6Info.Route = false
	for _, dns := range v6DNS {
		if probeUDP6(dns, 3*time.Second) {
			ipv6Info.Route = true
			break
		}
	}
	go refreshPublicIP() // 公网可达性后台查询（离线不阻塞启动）

	caPEM, err := ensureCerts("certs", addrs)
	if err != nil {
		log.Fatalf("证书初始化失败: %v", err)
	}

	h := newHub()
	logSinkInst.attach(h) // 绑定 hub：启动早期日志已暂存，此后实时转发日志窗口

	turnURL, turnUser, turnPass := "", "", ""
	if len(addrs) > 0 {
		if u, user, pass, err := startTURN(*turnPort, net.ParseIP(addrs[0])); err != nil {
			log.Printf("TURN 中继启动失败（远程观看将无 TCP 兜底）: %v", err)
		} else {
			turnURL, turnUser, turnPass = u, user, pass
		}
	}
	h.sfu.iceServers = buildICEServers(turnURL, turnUser, turnPass)
	go h.chatFileCleanup() // 聊天文件过期清理
	go h.rttProbe()        // 周期测量各用户到主机的延迟（卡片右上角角标）

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := indexHTML.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "page missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	mux.HandleFunc("/favicon.png", func(w http.ResponseWriter, r *http.Request) { // 网页标签页图标
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(iconPNG)
	})
	mux.HandleFunc("/ws", h.serveWS)
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/log-level", h.handleLogLevel)
	mux.HandleFunc("/api/chat-file", h.handleChatFileUpload)
	mux.HandleFunc("/api/chat-file/", h.handleChatFileGet)
	mux.HandleFunc("/api/chat-config", h.handleChatConfig)
	mux.HandleFunc("/api/link-preview", h.handleLinkPreview)
	mux.HandleFunc("/api/music/list", h.handleMusicList)
	mux.HandleFunc("/api/music/add", h.handleMusicAdd)
	mux.HandleFunc("/api/music/remove", h.handleMusicRemove)
	mux.HandleFunc("/api/music/removePlaylist", h.handleMusicRemovePlaylist)
	mux.HandleFunc("/api/music/refresh", h.handleMusicRefreshPlaylist)
	mux.HandleFunc("/api/music/search", h.handleMusicSearch)
	mux.HandleFunc("/api/music/preview", h.handleMusicPreview)
	mux.HandleFunc("/api/music/cover", h.handleMusicCover)
	mux.HandleFunc("/api/music/stream", h.handleMusicStream)
	mux.HandleFunc("/api/music/lyric", h.handleMusicLyric)
	mux.HandleFunc("/ca.crt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="teashare-ca.crt"`)
		w.Write(caPEM)
	})
	mux.HandleFunc("/addrs", func(w http.ResponseWriter, r *http.Request) {
		ice := []map[string]any{}
		if turnURL != "" {
			ice = append(ice, map[string]any{
				"urls":       []string{turnURL},
				"username":   turnUser,
				"credential": turnPass,
			})
		}
		ipv6Mu.RLock()
		ipv6 := ipv6Info
		ipv6Mu.RUnlock()
		json.NewEncoder(w).Encode(map[string]any{"addrs": addrs, "port": *port, "iceServers": ice, "myHostName": h.myHostName(), "version": appVersion, "serverStart": h.serverStart, "ipv6": ipv6})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf("[::]:%d", *port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	tlsCert, err := tls.LoadX509KeyPair(filepath.Join("certs", "server.crt"), filepath.Join("certs", "server.key"))
	if err != nil {
		log.Fatalf("加载服务器证书失败: %v", err)
	}
	srv.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", srv.Addr, err)
	}

	log.Println("==============================================")
	log.Println(" TeaRTC 屏幕共享服务已启动")
	log.Println("----------------------------------------------")
	if len(addrs) > 0 {
		log.Println(" 观看地址（IPv6，发给对方用浏览器打开）:")
		for _, a := range addrs {
			log.Printf("   https://[%s]:%d", a, *port)
		}
		log.Printf(" IPv6 出站可用:  %v", boolTxt(ipv6Info.Route))
		log.Println(" 公网 IPv6 检测:  后台进行中（可在页面「关于」查看主机网络状态）")
		log.Println(" 浏览端也需开启 IPv6 才能通过 IPv6 访问（可在页面「关于」复制提示给对方）")
	} else {
		log.Println(" !! 未检测到可用的全局 IPv6 地址，请检查网络")
	}
	log.Printf(" 本机自测:      https://127.0.0.1:%d", *port)
	log.Println(" CA 证书:       certs\\ca.crt （对方安装后访问无警告）")
	log.Println("----------------------------------------------")
	log.Printf(" 若对方连不上，请以管理员执行防火墙放行（脚本已自动尝试）:\n   netsh advfirewall firewall add rule name=\"TeaShare-%d\" dir=in action=allow protocol=TCP localport=%d\n   netsh advfirewall firewall add rule name=\"TeaShare-%d-UDP\" dir=in action=allow protocol=UDP localport=%d\n   netsh advfirewall firewall add rule name=\"TeaShare-turn-%d\" dir=in action=allow protocol=TCP localport=%d", *port, *port, *port, *port, *turnPort, *turnPort)
	log.Println("   WebRTC 画面走随机 UDP 端口，还需允许浏览器（chrome.exe）通过防火墙（设置 → 防火墙 → 允许应用通过）")
	if turnURL != "" {
		log.Printf("   TURN 中继(远程 UDP 被丢时的 TCP 兜底): %s", turnURL)
	} else {
		log.Println("   TURN 中继不可用（无 IPv6 或端口被占用）")
	}
	log.Println("==============================================")

	log.Printf("服务运行中: https://[::]:%d", *port)
	ensureFirewallRules(*port, *turnPort)
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil {
			log.Printf("HTTP 服务异常: %v", err)
		}
	}()
	runTray(*port) // 系统托盘（阻塞；X 关闭控制台 → 隐藏驻留托盘，托盘「退出」才结束进程）
}

// runHiddenCmd 执行 cmd /c 命令且不弹出控制台窗口（netsh/start 等后台命令用）
func runHiddenCmd(args ...string) *exec.Cmd {
	c := exec.Command("cmd", append([]string{"/c"}, args...)...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return c
}

// ensureFirewallRules 尽力自动放行入站（跨网络观看媒体链路必需；需管理员权限，失败明确提示手动命令）。
// 覆盖：程序规则（本 exe 全端口，WebRTC 随机 UDP + 自定义 -port 一次解决）、服务端口 TCP/UDP、TURN 控制面 TCP。
// 同步执行（删除/添加逐个等待完成），避免旧同名规则未删完导致 add 失败。
func ensureFirewallRules(svcPort, turnPort int) {
	// 规则已存在（安装版/管理员已运行过）→ 跳过，避免普通运行时反复尝试 netsh 刷屏
	if out, err := runHiddenCmd(`netsh advfirewall firewall show rule name="TeaShare-app"`).Output(); err == nil && strings.Contains(string(out), "TeaShare-app") {
		log.Println("防火墙规则已配置（TeaShare-app 存在），无需重复放行")
		return
	}
	type rule struct{ name, prot, lp string }
	rules := []rule{
		{fmt.Sprintf("TeaShare-%d", svcPort), "TCP", fmt.Sprint(svcPort)},
		{fmt.Sprintf("TeaShare-%d-UDP", svcPort), "UDP", fmt.Sprint(svcPort)},
		{"TeaShare-turn-3478", "TCP", fmt.Sprint(turnPort)},
	}
	if exe, err := os.Executable(); err == nil {
		delApp := fmt.Sprintf(`netsh advfirewall firewall delete rule name="TeaShare-app"`)
		runHiddenCmd(delApp).Output() // 等待删除完成
		addApp := fmt.Sprintf(`netsh advfirewall firewall add rule name="TeaShare-app" dir=in action=allow program="%s" enable=yes profile=any`, exe)
		if _, err := runHiddenCmd(addApp).Output(); err != nil {
			log.Printf("防火墙程序规则添加失败（请以管理员运行）: %v", err)
		} else {
			log.Println("防火墙规则已生效：TeaShare-app（程序全端口入站放行）")
		}
	}
	for _, r := range rules {
		del := fmt.Sprintf(`netsh advfirewall firewall delete rule name="%s"`, r.name)
		runHiddenCmd(del).Output() // 等待删除完成，避免 add 同名冲突
		add := fmt.Sprintf(`netsh advfirewall firewall add rule name="%s" dir=in action=allow protocol=%s localport=%s`, r.name, r.prot, r.lp)
		if _, err := runHiddenCmd(add).Output(); err != nil {
			log.Printf("防火墙规则 %s 添加失败（请以管理员运行）: %v", r.name, err)
		} else {
			log.Printf("防火墙规则已生效：%s（%s %s）", r.name, r.prot, r.lp)
		}
	}
	log.Println("防火墙放行已自动尝试；若未生效请以管理员身份运行程序一次，或手动执行上述 netsh 命令")
}

// ---------- IPv6 探测 ----------

// 出站探测目标（公网 DNS，UDP connect 不发包、无风险）
var v6DNS = []string{
	"2606:4700:4700::1111", // Cloudflare
	"2001:4860:4860::8888", // Google
}

// ipv6Status 主机 IPv6 检测结果（前端「关于」弹窗告知，注明为主机信息）
type ipv6Status struct {
	LocalAddrs []string `json:"localAddrs"` // 本机可用全局 IPv6 地址
	Route      bool     `json:"route"`      // 出站源地址可用（UDP6 connect 成功）
	Public     bool     `json:"public"`     // 公网 IPv6 可达
	PublicIPv6 string   `json:"publicIPv6"` // 公网 IPv6 地址
	PublicIPv4 string   `json:"publicIPv4"` // 公网 IPv4 地址
}

var (
	ipv6Mu   sync.RWMutex
	ipv6Info ipv6Status
)

// probeUDP6 UDP6 connect 出站探测（无包，等价 Python 源地址选择技巧；本机无 IPv6 直接报错）
func probeUDP6(host string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("udp6", net.JoinHostPort(host, "80"), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// queryPublicIP 公网 IP 查询（强制 IPv6/IPv4 传输；依赖外部 API，离线时返回空）
func queryPublicIP(ipv6 bool, timeout time.Duration) string {
	network := "tcp4"
	urls := []string{"https://api4.ipify.org", "https://4.ipw.cn/", "https://api.ip.sb/ip"}
	if ipv6 {
		network = "tcp6"
		urls = []string{"https://api6.ipify.org", "https://6.ipw.cn/", "https://api-ipv6.ip.sb/ip"}
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, addr) // 强制 tcp6/tcp4，用实际地址（此前传空地址导致必然失败）
		},
	}
	client := &http.Client{Timeout: timeout, Transport: tr}
	for _, url := range urls {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		s := strings.TrimSpace(string(b))
		if isValidIP(s, ipv6) {
			return s
		}
	}
	return ""
}

// isValidIP 校验返回内容确实是目标类型的 IP（防 API 返回错误页/多余文本）
func isValidIP(s string, ipv6 bool) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	return ipv6 == (ip.To4() == nil)
}

// refreshPublicIP 后台刷新公网可达性（不阻塞启动；公网状态变化慢，启动时查一次即可）
func refreshPublicIP() {
	p6 := queryPublicIP(true, 5*time.Second)
	p4 := queryPublicIP(false, 5*time.Second)
	ipv6Mu.Lock()
	ipv6Info.Public, ipv6Info.PublicIPv6 = p6 != "", p6
	ipv6Info.PublicIPv4 = p4
	ipv6Mu.Unlock()
	log.Printf("ipv6: 公网 IPv6=%s（可达=%v） 公网 IPv4=%s", orDash(p6), p6 != "", orDash(p4))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func boolTxt(b bool) string {
	if b {
		return "可用"
	}
	return "不可用"
}

func detectIPv6() []string {
	seen := make(map[string]bool)
	var list []string
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("枚举网卡失败: %v", err)
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() != nil {
				continue
			}
			ip := ipnet.IP
			if !usableIPv6(ip) {
				continue
			}
			s := ip.String()
			if seen[s] {
				continue
			}
			if ipv6SelfTest(ip) {
				seen[s] = true
				list = append(list, s)
			}
		}
	}
	sort.Strings(list)
	return list
}

func usableIPv6(ip net.IP) bool {
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip.Equal(net.ParseIP("::1")) {
		return false
	}
	if ip[0] == 0xfe && ip[1]&0xc0 == 0x80 { // fe80::/10 链路本地
		return false
	}
	if ip[0] == 0xfc || ip[0] == 0xfd { // fc00::/7 ULA 不跨网路由
		return false
	}
	if ip[0] == 0x20 && ip[1] == 0x01 {
		if ip[2] == 0x0d && ip[3] == 0xb8 { // 2001:db8::/32 文档地址
			return false
		}
		if ip[2] == 0 && ip[3] == 0 { // 2001::/32 Teredo
			return false
		}
	}
	if ip[0] == 0x20 && ip[1] == 0x02 { // 2002::/16 6to4
		return false
	}
	return true
}

// ipv6SelfTest 在指定地址上监听 TCP 端口并回连自身，验证地址真实可用（可绑定、协议栈就绪）
func ipv6SelfTest(ip net.IP) bool {
	ln, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: ip, Port: 0})
	if err != nil {
		return false
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	conn, err := net.DialTimeout("tcp6", net.JoinHostPort(ip.String(), fmt.Sprint(addr.Port)), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ---------- 证书 ----------

func ensureCerts(dir string, addrs []string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	caKey, caCert, err := loadOrCreateCA(filepath.Join(dir, "ca.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	if err := ensureServerCert(filepath.Join(dir, "server.key"), filepath.Join(dir, "server.crt"), caKey, caCert, addrs); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "ca.crt"))
}

func loadOrCreateCA(keyPath, certPath string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	if key, cert, err := loadKeyPair(keyPath, certPath); err == nil {
		return key, cert, nil
	}
	log.Println("未找到本地 CA，正在生成新的 CA 与证书（有效期 10 年，持久保存在 certs/）...")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: caCN, Organization: []string{caOrg}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(caValidDays, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return key, cert, saveKeyPair(keyPath, certPath, key, cert)
}

func ensureServerCert(keyPath, certPath string, caKey *ecdsa.PrivateKey, caCert *x509.Certificate, addrs []string) error {
	if _, cert, err := loadKeyPair(keyPath, certPath); err == nil {
		if time.Now().Before(cert.NotAfter.Add(-24*time.Hour)) && sanCovers(cert, addrs) {
			return nil
		}
	}
	log.Println("生成服务器证书（SAN 覆盖全部 IPv6 地址）...")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, a := range addrs {
		if ip := net.ParseIP(strings.TrimPrefix(a, "[")); ip != nil {
			ips = append(ips, ip)
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "teashare.local", Organization: []string{caOrg}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(srvValidDays, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     []string{"localhost", "teashare.local"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	return saveKeyPair(keyPath, certPath, key, cert)
}

func sanCovers(cert *x509.Certificate, addrs []string) bool {
	for _, a := range addrs {
		ip := net.ParseIP(a)
		found := false
		for _, cip := range cert.IPAddresses {
			if cip.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func saveKeyPair(keyPath, certPath string, key *ecdsa.PrivateKey, cert *x509.Certificate) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, certPEM, 0o644)
}

func loadKeyPair(keyPath, certPath string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("无效的私钥文件: %s", keyPath)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, nil, fmt.Errorf("无效的证书文件: %s", certPath)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	return key, cert, err
}

// ---------- TURN 中继 ----------

func randomCred(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ipv6RelayGen 是 pion/turn 的 RelayAddressGenerator 实现。
// pion/turn 对 TCP listener 的 allocation 内部使用 "udp4" 网络，
// 无法绑定 IPv6 中继地址（导致 "no suitable address found"），
// 因此忽略其 network 参数，强制用 "udp" 绑定到指定的 IPv6 地址。
type ipv6RelayGen struct {
	relayIP net.IP
}

func (g *ipv6RelayGen) Validate() error {
	if g.relayIP == nil {
		return fmt.Errorf("relay ip required")
	}
	return nil
}

func (g *ipv6RelayGen) AllocatePacketConn(_ string, requestedPort int) (net.PacketConn, net.Addr, error) {
	// 中继端口使用系统随机（requestedPort=0）：程序规则 TeaShare-app 已放行本 exe 全端口入站
	conn, err := net.ListenPacket("udp", fmt.Sprintf("[%s]:%d", g.relayIP, requestedPort))
	if err != nil {
		return nil, nil, err
	}
	relayAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		conn.Close()
		return nil, nil, fmt.Errorf("not a udp address")
	}
	relayAddr.IP = g.relayIP
	return conn, relayAddr, nil
}

func (g *ipv6RelayGen) AllocateConn(string, int) (net.Conn, net.Addr, error) {
	return nil, nil, fmt.Errorf("tcp relay unsupported")
}

// startTURN 启动 TURN over TCP 中继：远程观看者的 UDP 被运营商/光猫丢弃时，
// WebRTC 自动改走 TCP 经本服务器中继（浏览器 ⇄ 服务器为 TCP，服务器 ⇄ 共享者为本地 UDP）
// 凭据持久化到 certs/turn-cred.json：重启复用，旧页面持有的凭据不失效
func startTURN(port int, relayIP net.IP) (url, username, password string, err error) {
	credPath := filepath.Join("certs", "turn-cred.json")
	data, rerr := os.ReadFile(credPath)
	if rerr == nil {
		var saved struct {
			User string `json:"user"`
			Pass string `json:"pass"`
		}
		if json.Unmarshal(data, &saved) == nil && saved.User != "" && saved.Pass != "" {
			username, password = saved.User, saved.Pass
		}
	}
	if username == "" || password == "" {
		username = randomCred(4)
		password = randomCred(6)
		if wdata, werr := json.Marshal(map[string]string{"user": username, "pass": password}); werr == nil {
			_ = os.MkdirAll("certs", 0o755)
			_ = os.WriteFile(credPath, wdata, 0o600)
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", port))
	if err != nil {
		return "", "", "", err
	}
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: "teashare",
		AuthHandler: func(u, realm string, src net.Addr) ([]byte, bool) {
			if u == username {
				return turn.GenerateAuthKey(u, realm, password), true
			}
			return nil, false
		},
		EventHandler: turn.EventHandler{
			OnAuth: func(srcAddr, dstAddr net.Addr, protocol, u, realm, method string, verdict bool) {
				logV("TURN: 认证 %s 方法=%s 结果=%v 来源=%v", u, method, verdict, srcAddr)
			},
			OnAllocationCreated: func(srcAddr, dstAddr net.Addr, protocol, u, realm string, relayAddr net.Addr, requestedPort int) {
				log.Printf("TURN: 中继分配 %v → %v (用户 %s)", srcAddr, relayAddr, u)
			},
			OnAllocationError: func(srcAddr, dstAddr net.Addr, protocol, message string) {
				log.Printf("TURN: 分配错误 来源=%v %s", srcAddr, message)
			},
		},
		ListenerConfigs: []turn.ListenerConfig{{
			Listener: ln,
			RelayAddressGenerator: &ipv6RelayGen{
				relayIP: relayIP,
			},
		}},
	})
	if err != nil {
		ln.Close()
		return "", "", "", err
	}
	_ = srv
	return fmt.Sprintf("turn:[%s]:%d?transport=tcp", relayIP, port), username, password, nil
}

// ---------- WebSocket 信令（对等模型）与 SFU ----------

var palette = []string{"#e63946", "#f4a261", "#2a9d8f", "#457b9d", "#9b5de5", "#f15bb5", "#00bbf9", "#00f5d4", "#ffd166", "#06d6a0", "#7209b7", "#3a86ff"}

type hub struct {
	mu         sync.Mutex
	peers      map[string]*peer
	clientIDs  map[string]*peer // clientId -> peer（刷新/重连沿用身份）
	sfu        *SFU
	seq        int
	streamCfgs map[string]map[string]any // streamId -> 共享者最新配置

	chatHist   []map[string]any     // 聊天历史（最近 N 条，重启清空；新成员可见）
	chatFiles  map[string]*chatFile // 聊天文件缓存（内存，重启清空，过期清理）
	chatCfg    chatConfig           // 聊天配置（config/chat.json）
	chatLogMu  sync.Mutex           // 聊天持久化日志写锁
	chatLog    *os.File             // chat.jsonl 追加写（持久化开启时打开）
	chatTotalB int64                // 聊天文件总字节（超上限时清理最旧）

	serverStart int64 // 服务器启动时间戳（unix ms，前端刷新判断用）

	// 远程控制会话（P2-2）：targetOwnerID -> 控制权
	ctrlMu     sync.Mutex
	ctrlReqs   map[string]*ctrlReq // 待被控端确认的请求（targetOwnerID -> 请求）
	ctrlActive map[string]*ctrlReq // 已授权会话（targetOwnerID -> 控制器）

	// 日志窗口：环形缓冲 + 订阅连接（logsink.go）
	logBufMu sync.Mutex
	logBuf   []string        // 最近日志（环形，上限 logBufMax）
	logSubs  map[string]bool // peer id -> 已订阅（日志窗口打开中）

	// 音乐播放器（共享听歌）：服务器为权威播放器，任何用户可控制；无人 30s 自动暂停
	musicMu           sync.Mutex
	musicList         []MusicItem       // 播放列表（持久化 config/music.json）
	musicIndex        int               // 当前播放索引（-1 = 无）
	musicScope        string            // 播放范围："all"（全部）/"single"（单曲与直链）/ 歌单 id
	musicOrder        string            // 播放顺序："sequence"（顺序）/"shuffle"（随机）/"loop"（单曲循环）
	musicPlay         bool              // 播放中
	musicPos          float64           // 暂停时进度（秒）
	musicAt           int64             // 开始播放时间戳（ms，进度 = musicPos + (now-musicAt)/1000）
	musicNoPeer       time.Time         // 上次有连接的时间（无人判定）
	musicCache        map[string][]byte // 流内存缓存（最近 musicCacheMax 首，防多用户重复拉网易云）
	musicCacheOrder   []string          // 流缓存 LRU 顺序（url）
	musicEnded        map[string]bool   // 直链曲目 ended 上报去重（URL -> 已上报）
	musicURLRefreshed map[string]int64  // 曲目直链刷新时间戳（id -> ms，10 分钟节流）
	musicHistory      []int             // 播放历史（最近 5 首的全局索引；随机模式"上一曲"用）
	musicForward      []int             // 随机"上一曲"撤销栈（prev 记录回退前曲；next 撤销回）
	musicShuffleQueue []int             // 随机播放洗牌队列（Fisher-Yates；播完重洗，保证不重复）
	coverRefreshMu    sync.Mutex        // 封面失效刷新去重/节流
	coverRefreshing   map[string]bool   // URL -> 刷新进行中（并发去重）
	coverRefreshTried map[string]int64  // URL -> 上次刷新尝试时间戳（5 分钟节流，防反复请求网易云）
}

// ctrlReq 远程控制请求/会话
type ctrlReq struct {
	controllerID   string
	controllerName string
	at             time.Time
}

// chatFile 聊天上传的文件（大文件落盘，小文件内存缓存）
type chatFile struct {
	data []byte // 小文件内存数据（big 时为 nil，从磁盘读）
	path string // 落盘路径（big 时有效）
	mime string
	name string
	at   time.Time
	big  bool // 大文件（> 阈值）：按时间过期清理；小文件随聊天历史上限清理
}

// chatConfig 聊天配置（config/chat.json；重启生效）
type chatConfig struct {
	HistKeep     int  `json:"histKeep"`     // 历史保留条数（内存+新成员补发）
	FileTotal    int  `json:"fileTotal"`    // 文件总缓存上限 MB
	FileExpire   int  `json:"fileExpire"`   // 大文件过期分钟
	FileMax      int  `json:"fileMax"`      // 单文件上限 MB
	BigThreshold int  `json:"bigThreshold"` // 大文件阈值 MB（大于此值落盘+过期清理）
	MsgLimit     int  `json:"msgLimit"`     // 消息文本字符上限
	Persist      bool `json:"persist"`      // 消息持久化开关
}

type SFU struct {
	hub        *hub
	mu         sync.Mutex
	streams    map[string]*stream // streamID(=ownerID) -> stream
	viewers    map[string]*viewerConn
	iceServers []webrtc.ICEServer
}

type peer struct {
	hub    *hub
	id     string
	name   string
	color  string
	conn   *websocket.Conn
	send   chan []byte
	closed bool

	clientID  string // 浏览器持久身份（刷新/重连沿用）
	replaced  bool   // 已被新连接顶替（unregister 时跳过广播）
	host      bool   // 服务器主机（本机运行的浏览器，标记"服务器主机"徽章）
	ip        string // 连接来源 IP（RemoteAddr，供观看端展示）
	enterHost string // 进入页面的 URL 主机（前端 location.hostname 上报，供观看端展示服务地址）
	hostId    string // 服务运行者 peer id（本服务器上的服务用户）
	joinedAt  int64  // 加入时刻（Unix 毫秒；供前端在线时长徽章，peer.mu 保护）
	rttMs     int64  // 该用户到其所在主机的延迟（服务器 ping 测量，peer.mu 保护）
	lastPong  int64  // 最近一次收到 pong 的时间戳（死连接探测，peer.mu 保护）

	mu          sync.Mutex
	pubPC       *webrtc.PeerConnection            // 屏幕/视频推流连接
	camPC       *webrtc.PeerConnection            // 摄像头推流连接（独立流，流 id = ownerID-cam）
	audioPubPCs map[string]*webrtc.PeerConnection // 音频推流连接（kind: mic/system，可共存）
}

type stream struct {
	ID           string
	OwnerID      string
	SourcePC     *webrtc.PeerConnection
	Codec        webrtc.RTPCodecCapability
	Layers       map[string]*rewriteTrack       // rid(l/m/h) -> local（视频 simulcast）
	Remote       map[string]*webrtc.TrackRemote // rid -> remote（视频）
	AudioTracks  map[string]*rewriteTrack       // 音频通道(kind: mic/system) -> local（可共存）
	AudioRemotes map[string]*webrtc.TrackRemote // kind -> remote
}

type viewerConn struct {
	peer        *peer
	pc          *webrtc.PeerConnection
	senders     map[string]*webrtc.RTPSender // streamID -> video sender
	audioSender map[string]*webrtc.RTPSender // streamID -> audio sender
	tiers       map[string]string            // streamID -> rid
	negCh       chan struct{}
	pmu         sync.Mutex
	pending     bool // 有在途 offer 未 answer 时标记待协商
}

func buildICEServers(turnURL, user, pass string) []webrtc.ICEServer {
	if turnURL == "" {
		return nil
	}
	return []webrtc.ICEServer{{URLs: []string{turnURL}, Username: user, Credential: pass}}
}

func newHub() *hub {
	h := &hub{
		peers:      make(map[string]*peer),
		clientIDs:  make(map[string]*peer),
		ctrlReqs:   make(map[string]*ctrlReq),
		ctrlActive: make(map[string]*ctrlReq),
		logSubs:    make(map[string]bool),
	}
	h.sfu = &SFU{hub: h, streams: make(map[string]*stream), viewers: make(map[string]*viewerConn)}
	h.chatFiles = make(map[string]*chatFile)
	h.loadChatConfig()
	h.loadChatHist() // 从磁盘加载最近历史
	h.musicIndex = -1
	h.musicCache = make(map[string][]byte)
	h.musicEnded = make(map[string]bool)
	h.musicURLRefreshed = make(map[string]int64)
	h.coverRefreshing = make(map[string]bool)
	h.coverRefreshTried = make(map[string]int64)
	h.serverStart = time.Now().UnixMilli()
	h.loadMusicState()
	go h.musicTicker()
	return h
}

func newPeerConnection(iceServers []webrtc.ICEServer) (*webrtc.PeerConnection, error) {
	return webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
}

func (p *peer) sharing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pubPC != nil
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var hostAddrsOnce sync.Once
var hostAddrs []string

// myHostName 本机服务运行者昵称（host 浏览器用户；无则空串，前端默认"主机"）
func (h *hub) myHostName() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.peers {
		if p.conn != nil && p.host {
			return p.name
		}
	}
	return ""
}

// isHostConn 判断连接来源是否本机（服务器主机：loopback 或本服务监听地址）
func isHostConn(r *http.Request) bool {
	// 穿透隧道（樱花/ngrok 等）把远程访问者转发到本地，连接源为回环但会带代理头
	//（X-Forwarded-For / X-Real-IP）——此时是远程浏览者而非本机用户：
	// 否则会误标「服务器主机」徽章，且 /api/log-level 等本机接口被穿透访问误放行
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	host := r.RemoteAddr
	if host == "" {
		return false
	}
	hstr, _, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	ip := net.ParseIP(hstr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	hostAddrsOnce.Do(func() { hostAddrs = detectIPv6() })
	for _, a := range hostAddrs {
		if p := net.ParseIP(a); p != nil && p.Equal(ip) {
			return true
		}
	}
	return false
}

// GET/POST /api/log-level：查询/切换日志调试模式（仅本机，运行时生效无需重启）
func (h *hub) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if !isHostConn(r) {
		http.Error(w, "仅本机可操作", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Verbose bool `json:"verbose"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		verboseFlag.Store(body.Verbose)
		log.Printf("日志模式切换为: %v", body.Verbose)
	}
	json.NewEncoder(w).Encode(map[string]any{"verbose": verboseFlag.Load()})
}

// ---------- 延迟探测（卡片"与主机的延迟"） ----------

// rttProbe 周期测量各用户到其主机的延迟：服务器主动 ping 本地浏览器（卡片右上角角标）
func (h *hub) rttProbe() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixMilli()
		h.mu.Lock()
		local := make([]*peer, 0, len(h.peers))
		for _, p := range h.peers {
			if p.conn != nil {
				local = append(local, p)
			}
		}
		h.mu.Unlock()
		for _, p := range local {
			sendMsg(p, map[string]any{"type": "ping", "t": now})
		}
		// 死连接探测：连续 20s 无 pong 的浏览器连接主动断开（触发其本地立即重连），
		// 避免半死连接挂到系统 TCP 超时——服务进程消失/手机网络半断场景
		nowMs := time.Now().UnixMilli()
		for _, p := range local {
			p.mu.Lock()
			dead := nowMs-p.lastPong > 20000 && p.lastPong > 0
			age := nowMs - p.lastPong
			p.mu.Unlock()
			if dead {
				log.Printf("peer: %s(%s) 心跳超时（%dms 无响应），主动断开", p.id, p.name, age)
				p.conn.Close() // 触发 readLoop 退出 -> unregister 清理
			}
		}
		// 用户延迟广播（卡片右上角角标）
		h.mu.Lock()
		rtts := make(map[string]any, len(h.peers))
		for id, p := range h.peers {
			p.mu.Lock()
			r := p.rttMs
			p.mu.Unlock()
			if r > 0 {
				rtts[id] = r
			}
		}
		h.mu.Unlock()
		if len(rtts) > 0 {
			h.broadcast(map[string]any{"type": "peer-rtts", "rtts": rtts})
		}
	}
}

// clientIP 提取连接来源 IP（去端口）
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func (h *hub) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	p := &peer{hub: h, conn: conn, send: make(chan []byte, 32), host: isHostConn(r), ip: clientIP(r), joinedAt: time.Now().UnixMilli()}
	go p.writeLoop()
	p.readLoop()
}

func (p *peer) readLoop() {
	defer func() {
		p.hub.unregister(p)
		p.conn.Close()
	}()
	for {
		_, data, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		p.handle(m)
	}
}

func (p *peer) writeLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-p.send:
			if !ok {
				return
			}
			if err := p.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (p *peer) handle(m map[string]any) {
	switch m["type"] {
	case "hello":
		p.handleHello(m)
	case "publish-offer":
		p.handlePublishOffer(m)
	case "subscribe-answer":
		p.handleSubscribeAnswer(m)
	case "candidate":
		p.handleCandidate(m)
	case "share-stop":
		p.hub.sfu.removeStreamVideo(p.id, nil) // 停止屏幕共享：仅移除视频，保留独立音频通道
	case "share-config":
		p.handleShareConfig(m)
	case "viewer-config":
		p.handleViewerConfig(m)
	case "chat":
		p.handleChat(m)
	case "ping":
		sendMsg(p, map[string]any{"type": "pong", "t": m["t"]})
	case "resubscribe": // 前端订阅 ICE 失败：重建订阅连接（P2P 会话不受影响）
		p.hub.sfu.resetSubscriber(p.id)
	case "ctrl":
		p.handleCtrl(m)
	case "music-ctl":
		p.hub.handleMusicCtl(m)
	case "log-subscribe": // 打开日志窗口：开始接收服务器日志
		p.hub.logSubscribe(p)
	case "log-unsubscribe": // 关闭日志窗口
		p.hub.logUnsubscribe(p)
	case "pong":
		if t, ok := m["t"].(float64); ok {
			now := time.Now().UnixMilli()
			p.mu.Lock()
			p.rttMs = now - int64(t)
			p.lastPong = now
			p.mu.Unlock()
		}
	}
}

// ---- 音乐播放器（共享听歌）：服务器权威播放器，任何用户可控制 ----

const musicCacheMax = 5 // 流内存缓存最近 5 首（防多用户重复拉网易云，重启清空）

// musicTicker 1s 定时器：播放中进度超曲目时长自动切歌
func (h *hub) musicTicker() {
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		h.musicMu.Lock()
		if h.musicPlay && h.musicIndex >= 0 && h.musicIndex < len(h.musicList) {
			cur := h.musicList[h.musicIndex]
			pos := h.musicPos + float64(time.Now().UnixMilli()-h.musicAt)/1000
			if cur.Dur > 0 && pos >= float64(cur.Dur) {
				h.musicMu.Unlock()
				h.musicAutoNext()
				continue
			}
		}
		h.musicMu.Unlock()
	}
}

// musicCtl 处理任意用户的控制消息（play/pause/next/prev/seek）
func (h *hub) handleMusicCtl(m map[string]any) {
	action, _ := m["action"].(string)
	switch action {
	case "play":
		h.musicSetPlay(true)
	case "pause":
		h.musicSetPlay(false)
	case "next":
		h.musicNext()
	case "prev":
		h.musicPrev()
	case "seek":
		pos, _ := m["pos"].(float64)
		if pos < 0 {
			pos = 0
		}
		h.musicMu.Lock()
		h.musicPos = pos
		h.musicAt = time.Now().UnixMilli()
		h.musicMu.Unlock()
		h.musicBroadcast()
	case "ended":
		// 直链无时长：观众 audio ended 上报切歌（同曲目去重）
		u, _ := m["url"].(string)
		h.musicMu.Lock()
		if h.musicPlay && h.musicIndex >= 0 && h.musicIndex < len(h.musicList) {
			if h.musicList[h.musicIndex].URL == u && !h.musicEnded[u] {
				h.musicEnded[u] = true
				h.musicMu.Unlock()
				h.musicAutoNext()
				return
			}
		}
		h.musicMu.Unlock()
	case "add":
		u, _ := m["url"].(string)
		if u != "" {
			go h.musicAdd(u)
		}
	case "remove":
		idx, _ := m["index"].(float64)
		h.musicRemove(int(idx))
	case "scope":
		// 切换播放范围：全部 / 单曲（single）/ 某歌单 id
		scope, _ := m["scope"].(string)
		h.musicSetScope(scope)
	case "order":
		// 切换播放顺序：sequence → shuffle → loop → sequence …
		h.musicCycleOrder()
	case "playAt":
		// 点击播放指定歌曲（全局 index）
		idx, _ := m["index"].(float64)
		h.musicPlayAt(int(idx))
	}
}

// musicScopeIndices 当前 scope 内曲目的全局索引列表（锁内调用；scope 无效回退 all）
func (h *hub) musicScopeIndices() []int {
	if h.musicScope == "all" {
		var idx []int
		seen := make(map[string]bool)
		for i, it := range h.musicList {
			k := dedupKey(it)
			if seen[k] {
				continue
			}
			seen[k] = true
			idx = append(idx, i)
		}
		return idx
	}
	var idx []int
	for i, it := range h.musicList {
		if h.musicScope == "single" {
			if it.PlaylistID == "" {
				idx = append(idx, i)
			}
		} else if it.PlaylistID == h.musicScope {
			idx = append(idx, i)
		}
	}
	return idx
}

// musicSetScope 切换播放范围：当前曲目不在新 scope 则跳到新 scope 第一首（保持播放状态）
func (h *hub) musicSetScope(scope string) {
	if scope == "" {
		scope = "all"
	}
	h.musicMu.Lock()
	h.musicScope = scope
	idx := h.musicScopeIndices()
	if len(idx) == 0 {
		h.musicScope = "all" // 空 scope 回退全部
	}
	h.musicShuffleQueue = nil // scope 变化：洗牌队列作废（下次随机重建）
	// 切换歌单仅改变播放范围，不打断当前播放（当前曲目继续播，直到用户操作）
	h.saveMusicStateLocked()
	h.musicMu.Unlock()
	h.musicBroadcast()
}

// musicCycleOrder 循环切换播放顺序：sequence → shuffle → loop → sequence
func (h *hub) musicCycleOrder() {
	h.musicMu.Lock()
	switch h.musicOrder {
	case "sequence":
		h.musicOrder = "shuffle"
	case "shuffle":
		h.musicOrder = "loop"
	default:
		h.musicOrder = "sequence"
	}
	h.saveMusicStateLocked()
	h.musicMu.Unlock()
	h.musicBroadcast()
}

func (h *hub) musicSetPlay(play bool) {
	h.musicMu.Lock()
	if play && h.musicIndex < 0 && len(h.musicList) > 0 {
		// 取当前 scope 内第一首（而非无条件 index=0）
		idx := h.musicScopeIndices()
		if len(idx) > 0 {
			h.musicIndex = idx[0]
		} else {
			h.musicIndex = 0
		}
	}
	if play {
		h.musicPlay = true
		h.musicAt = time.Now().UnixMilli()
		h.musicEnded = make(map[string]bool)
	} else {
		h.musicPlay = false
		if h.musicIndex >= 0 {
			h.musicPos += float64(time.Now().UnixMilli()-h.musicAt) / 1000
		}
	}
	h.musicMu.Unlock()
	h.musicBroadcast()
}

// musicPlayAt 跳到指定 index 播放（点击列表曲目）
func (h *hub) musicPlayAt(idx int) {
	h.musicMu.Lock()
	if idx < 0 || idx >= len(h.musicList) {
		h.musicMu.Unlock()
		return
	}
	oldIdx := h.musicIndex
	if idx != oldIdx {
		h.musicPushHistory(oldIdx) // 旧曲（已播过）入历史
	}
	h.musicForward = nil // 手动选曲：撤销栈作废
	h.musicIndex = idx
	h.musicPos = 0
	h.musicAt = time.Now().UnixMilli()
	h.musicPlay = true
	h.musicEnded = make(map[string]bool)
	h.saveMusicStateLocked()
	h.musicMu.Unlock()
	h.musicBroadcast()
}

func (h *hub) musicNext() {
	h.musicMu.Lock()
	oldIdx := h.musicIndex // 播过的旧曲（历史入栈，随机"上一曲"回退用）
	idx := h.musicScopeIndices()
	if len(idx) == 0 {
		h.musicMu.Unlock()
		return
	}
	if h.musicOrder == "shuffle" {
		// 随机播放：优先撤销"上一曲"（回到回退前的歌）；否则随机选一首（排除当前）
		if len(h.musicForward) > 0 {
			h.musicIndex = h.musicForward[len(h.musicForward)-1]
			h.musicForward = h.musicForward[:len(h.musicForward)-1]
			h.musicPos = 0
			h.musicAt = time.Now().UnixMilli()
			h.musicPlay = true
			h.musicEnded = make(map[string]bool)
		} else if len(idx) == 1 {
			h.musicPos = 0
			h.musicAt = time.Now().UnixMilli()
			h.musicPlay = true
			h.musicEnded = make(map[string]bool)
		} else {
			// 洗牌队列随机（播完重洗；排除当前，保证全部播完前不重复）
			h.musicIndex = h.popShuffle(h.musicIndex)
			h.musicPos = 0
			h.musicAt = time.Now().UnixMilli()
			h.musicPlay = true
			h.musicEnded = make(map[string]bool)
		}
	} else {
		h.musicForward = nil // 顺序/循环切歌：撤销栈作废（新轨迹）
		// 顺序播放（含单曲循环下手动切歌也切）：scope 内下一位
		pos := -1
		for i, gi := range idx {
			if gi == h.musicIndex {
				pos = i
				break
			}
		}
		// 去重后当前曲目不在 idx 中（重复项被去掉）→ 按去重键找真实位置
		if pos == -1 {
			curKey := dedupKey(h.musicList[h.musicIndex])
			for i, gi := range idx {
				if dedupKey(h.musicList[gi]) == curKey {
					pos = i
					break
				}
			}
		}
		if pos == -1 {
			pos = 0
		}
		h.musicIndex = idx[(pos+1)%len(idx)]
		h.musicPos = 0
		h.musicAt = time.Now().UnixMilli()
		h.musicPlay = true
		h.musicEnded = make(map[string]bool)
	}
	h.musicPushHistory(oldIdx) // 旧曲（已播过）入历史
	h.musicMu.Unlock()
	h.musicBroadcast()
}

// musicAutoNext 自动切歌（播完超时/ended 上报）：单曲循环时重播当前，其余同手动切歌
func (h *hub) musicAutoNext() {
	h.musicMu.Lock()
	if h.musicOrder == "loop" && h.musicIndex >= 0 && h.musicIndex < len(h.musicList) {
		h.musicPos = 0
		h.musicAt = time.Now().UnixMilli()
		h.musicPlay = true
		h.musicEnded = make(map[string]bool)
		h.musicMu.Unlock()
		h.musicBroadcast()
		return
	}
	h.musicMu.Unlock()
	h.musicNext()
}

// musicPushHistory 播放历史入栈（去重相邻 + 上限 5；需持 musicMu）
func (h *hub) musicPushHistory(idx int) {
	if idx < 0 {
		return
	}
	if len(h.musicHistory) > 0 && h.musicHistory[len(h.musicHistory)-1] == idx {
		return // 相邻同曲不入
	}
	h.musicHistory = append(h.musicHistory, idx)
	if len(h.musicHistory) > 5 {
		h.musicHistory = h.musicHistory[len(h.musicHistory)-5:]
	}
}

// rebuildShuffleQueue 重建随机播放洗牌队列（当前 scope 曲目 Fisher-Yates 洗牌；需持 musicMu）
func (h *hub) rebuildShuffleQueue() {
	idx := h.musicScopeIndices()
	q := make([]int, len(idx))
	copy(q, idx)
	for i := len(q) - 1; i > 0; i-- { // Fisher-Yates
		j := mrand.Intn(i + 1)
		q[i], q[j] = q[j], q[i]
	}
	h.musicShuffleQueue = q
}

// popShuffle 取随机队列下一首（排除当前曲；队列耗尽自动重建；需持 musicMu）
func (h *hub) popShuffle(cur int) int {
	for len(h.musicShuffleQueue) > 0 {
		last := len(h.musicShuffleQueue) - 1
		v := h.musicShuffleQueue[last]
		h.musicShuffleQueue = h.musicShuffleQueue[:last]
		if v != cur {
			return v
		}
	}
	h.rebuildShuffleQueue()
	if len(h.musicShuffleQueue) > 0 {
		last := len(h.musicShuffleQueue) - 1
		v := h.musicShuffleQueue[last]
		h.musicShuffleQueue = h.musicShuffleQueue[:last]
		return v
	}
	return cur
}

func (h *hub) musicPrev() {
	h.musicMu.Lock()
	// 随机播放："上一曲" = 历史上一曲（最近 5 首内回退）
	if h.musicOrder == "shuffle" && len(h.musicHistory) > 0 {
		prev := h.musicHistory[len(h.musicHistory)-1]
		h.musicHistory = h.musicHistory[:len(h.musicHistory)-1]
		h.musicForward = append(h.musicForward, h.musicIndex) // 记录回退前曲（next 可撤销回）
		h.musicIndex = prev
		h.musicPos = 0
		h.musicAt = time.Now().UnixMilli()
		h.musicPlay = true
		h.musicEnded = make(map[string]bool)
		h.musicMu.Unlock()
		h.musicBroadcast()
		return
	}
	idx := h.musicScopeIndices()
	if len(idx) == 0 {
		h.musicMu.Unlock()
		return
	}
	pos := -1
	for i, gi := range idx {
		if gi == h.musicIndex {
			pos = i
			break
		}
	}
	// 去重后当前曲目不在 idx 中（重复项被去掉）→ 按去重键找真实位置
	if pos == -1 {
		curKey := dedupKey(h.musicList[h.musicIndex])
		for i, gi := range idx {
			if dedupKey(h.musicList[gi]) == curKey {
				pos = i
				break
			}
		}
	}
	if pos == -1 {
		pos = 0
	}
	h.musicIndex = idx[(pos-1+len(idx))%len(idx)]
	h.musicPos = 0
	h.musicAt = time.Now().UnixMilli()
	h.musicPlay = true
	h.musicEnded = make(map[string]bool)
	h.musicMu.Unlock()
	h.musicBroadcast()
}

// musicAdd 解析链接入列（异步，成功后广播列表）
func (h *hub) musicAdd(u string) {
	items, err := parseMusicURL(u)
	if err != nil || len(items) == 0 {
		return
	}
	// 提取 PlaylistID（歌单有；单曲/直链为空）
	pid := ""
	for _, it := range items {
		if it.PlaylistID != "" {
			pid = it.PlaylistID
			break
		}
	}
	if h.musicScope == "" {
		h.musicScope = "all"
	}
	h.musicReplacePlaylist(pid, items)
}

func (h *hub) musicRemove(idx int) {
	h.musicMu.Lock()
	if idx < 0 || idx >= len(h.musicList) {
		h.musicMu.Unlock()
		return
	}
	h.musicList = append(h.musicList[:idx], h.musicList[idx+1:]...)
	if len(h.musicList) == 0 {
		h.musicIndex = -1
		h.musicPlay = false
	} else if h.musicIndex == idx {
		h.musicIndex = idx % len(h.musicList)
		h.musicPos = 0
		h.musicAt = time.Now().UnixMilli()
	} else if h.musicIndex > idx {
		h.musicIndex--
	}
	h.saveMusicStateLocked()
	h.musicMu.Unlock()
	h.musicBroadcast()
}

// musicRemovePlaylist 移除整个歌单或单曲（scope = playlistId / "single"）
func (h *hub) musicRemovePlaylist(scope string) {
	h.musicMu.Lock()
	if scope == "" {
		h.musicMu.Unlock()
		return
	}
	var kept []MusicItem
	for _, it := range h.musicList {
		if scope == "single" {
			if it.PlaylistID == "" {
				continue
			}
		} else if it.PlaylistID == scope {
			continue
		}
		kept = append(kept, it)
	}
	if len(kept) == len(h.musicList) {
		h.musicMu.Unlock()
		return
	}
	h.musicList = kept
	if len(h.musicList) == 0 {
		h.musicIndex = -1
		h.musicPlay = false
		h.musicPos = 0
		h.musicAt = time.Now().UnixMilli()
		h.musicScope = "all"
	} else if h.musicIndex >= len(h.musicList) {
		h.musicIndex = len(h.musicList) - 1
		h.musicPos = 0
		h.musicAt = time.Now().UnixMilli()
	}
	h.saveMusicStateLocked()
	h.musicMu.Unlock()
	h.musicBroadcast()
}

// musicReplacePlaylist 替换歌单：删除旧曲 + 插入新曲（原位置）+ 修正 index/scope
// pid 空 = 单曲追加（带 URL 去重）；pid 有 = 歌单替换
func (h *hub) musicReplacePlaylist(pid string, items []MusicItem) {
	h.musicMu.Lock()
	if pid == "" {
		// 单曲/直链：URL 去重后追加
		seen := make(map[string]bool, len(h.musicList))
		for _, it := range h.musicList {
			if it.Source == "direct" {
				seen[it.URL] = true
			}
		}
		added := 0
		for _, it := range items {
			if it.Source == "direct" && seen[it.URL] {
				continue
			}
			h.musicList = append(h.musicList, it)
			added++
		}
		if added > 0 && h.musicIndex < 0 {
			h.musicIndex = 0
		}
		h.saveMusicStateLocked()
		h.musicMu.Unlock()
		h.musicBroadcast()
		return
	}
	// 歌单：找到旧曲起始位置，删除旧曲，在原位插入新曲
	insertPos := -1
	var kept []MusicItem
	for i, it := range h.musicList {
		if it.PlaylistID == pid {
			if insertPos == -1 {
				insertPos = i
			}
			continue
		}
		kept = append(kept, it)
	}
	if insertPos == -1 {
		insertPos = len(kept)
	}
	// 拼接：前半 + 新曲 + 后半
	final := make([]MusicItem, 0, len(kept)+len(items))
	final = append(final, kept[:insertPos]...)
	final = append(final, items...)
	final = append(final, kept[insertPos:]...)
	h.musicList = final
	h.musicShuffleQueue = nil // 列表变化：洗牌队列作废（下次随机重建）
	// 修正 musicIndex
	if len(h.musicList) == 0 {
		h.musicIndex = -1
		h.musicPlay = false
	} else if len(items) > 0 {
		// 尝试保持当前曲目（按 ID 匹配新列表）
		if h.musicIndex >= 0 && h.musicIndex < len(kept) {
			curID := kept[h.musicIndex].ID
			found := false
			if curID != "" {
				for i := insertPos; i < insertPos+len(items) && i < len(h.musicList); i++ {
					if h.musicList[i].ID == curID {
						h.musicIndex = i
						found = true
						break
					}
				}
			}
			if !found {
				h.musicIndex = insertPos
			}
		} else if h.musicIndex >= len(h.musicList) {
			h.musicIndex = len(h.musicList) - 1
		}
		h.musicPos = 0
		h.musicAt = time.Now().UnixMilli()
	} else if h.musicIndex >= len(h.musicList) {
		h.musicIndex = len(h.musicList) - 1
	}
	// 校验 scope
	if h.musicScope != "all" && h.musicScope != "single" {
		found := false
		for _, it := range h.musicList {
			if it.PlaylistID == h.musicScope {
				found = true
				break
			}
		}
		if !found {
			h.musicScope = "all"
		}
	}
	h.saveMusicStateLocked()
	h.musicMu.Unlock()
	h.musicBroadcast()
}

// musicState 当前状态快照（锁外调用需自行持锁）
func (h *hub) musicState() map[string]any {
	h.musicMu.Lock()
	defer h.musicMu.Unlock()
	st := map[string]any{
		"list": h.musicList, "index": h.musicIndex, "playing": h.musicPlay,
		"pos": h.musicPos, "at": h.musicAt, "scope": h.musicScope, "order": h.musicOrder,
	}
	return st
}

// musicBroadcast 广播音乐状态（变化时/周期同步）；online = 当前在线浏览器用户数（一起听人数）
func (h *hub) musicBroadcast() {
	online := h.musicPeerCount()
	h.broadcast(map[string]any{"type": "music-state", "state": h.musicState(), "online": online})
}

// musicPeerCount 当前在线浏览器用户数（无人判定）
func (h *hub) musicPeerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.peers {
		if p.conn != nil {
			n++
		}
	}
	return n
}

// musicNoPeerTick 连接变化时调用：无人（WS=0）30s 后自动暂停
func (h *hub) musicNoPeerTick() {
	if h.musicPeerCount() > 0 {
		return
	}
	if h.musicNoPeer.IsZero() {
		h.musicNoPeer = time.Now()
		go func() {
			time.Sleep(30 * time.Second)
			h.musicMu.Lock()
			again := h.musicPeerCount() == 0 && h.musicPlay
			h.musicMu.Unlock()
			if again {
				h.musicSetPlay(false)
				log.Printf("music: 房间无人 30s，自动暂停播放")
			}
		}()
	} else if time.Since(h.musicNoPeer) > 30*time.Second {
		h.musicNoPeer = time.Time{} // 有连接恢复：重置无人计时
	}
}

// saveMusicStateLocked 持久化列表+状态（config/music.json；需已持 musicMu）
func (h *hub) saveMusicStateLocked() {
	data, err := json.Marshal(map[string]any{"list": h.musicList, "index": h.musicIndex, "pos": h.musicPos, "playing": h.musicPlay, "scope": h.musicScope, "order": h.musicOrder})
	if err != nil {
		return
	}
	_ = os.MkdirAll("config", 0o755)
	_ = os.WriteFile("config/music.json", data, 0o600)
}

// loadMusicState 启动加载持久化状态
func (h *hub) loadMusicState() {
	data, err := os.ReadFile("config/music.json")
	if err != nil {
		return
	}
	var m struct {
		List    []MusicItem `json:"list"`
		Index   int         `json:"index"`
		Pos     float64     `json:"pos"`
		Playing bool        `json:"playing"`
		Scope   string      `json:"scope"`
		Order   string      `json:"order"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	h.musicMu.Lock()
	h.musicList = m.List
	h.musicIndex = m.Index
	h.musicPos = m.Pos
	h.musicScope = m.Scope
	if h.musicScope == "" {
		h.musicScope = "all"
	}
	h.musicOrder = m.Order
	if h.musicOrder == "" {
		h.musicOrder = "sequence"
	}
	h.musicPlay = false // 重启后不自动播放（等用户操作）
	h.musicMu.Unlock()
}

// ---- 远程控制（P2-2）：仅支持控制"本机 host（服务器运行者）的屏幕"，被控端确认后单一控制者 ----

func (p *peer) handleCtrl(m map[string]any) {
	action, _ := m["action"].(string)
	switch action {
	case "request":
		target, _ := m["target"].(string)
		p.hub.ctrlRequest(p, target)
	case "respond":
		target, _ := m["target"].(string)
		accept, _ := m["accept"].(bool)
		p.hub.ctrlRespond(p, target, accept)
	case "invite": // 被控端（host）邀请指定观看者控制
		target, _ := m["target"].(string)
		p.hub.ctrlInvite(p, target)
	case "invite-respond": // 被邀请者回应
		accept, _ := m["accept"].(bool)
		p.hub.ctrlInviteRespond(p, accept)
	case "release":
		p.hub.ctrlRelease(p)
	case "revoke":
		p.hub.ctrlRevoke(p)
	case "mouse":
		if !p.hub.ctrlIsController(p) {
			return
		}
		x, _ := m["x"].(float64)
		y, _ := m["y"].(float64)
		ax, ay := p.hub.ctrlAbsCoords(p, int(x), int(y))
		ctrlMouseMove(ax, ay)
	case "click":
		if !p.hub.ctrlIsController(p) {
			return
		}
		button, _ := m["button"].(string)
		down, _ := m["down"].(bool)
		ctrlMouseButton(button, down)
	case "scroll":
		if !p.hub.ctrlIsController(p) {
			return
		}
		dy, _ := m["dy"].(float64)
		ctrlMouseWheel(int(dy))
	case "key":
		if !p.hub.ctrlIsController(p) {
			return
		}
		code, _ := m["code"].(string)
		down, _ := m["down"].(bool)
		ctrlKey(code, down)
	}
}

func (h *hub) ctrlRequest(p *peer, target string) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	tgt := h.peers[target]
	if tgt == nil {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "request-result", "ok": false, "reason": "对方不在线"})
		return
	}
	if target == p.id {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "request-result", "ok": false, "reason": "不能控制自己"})
		return
	}
	if !tgt.host {
		// 只有服务器运行者（本机浏览器）的屏幕可被控（SendInput 执行在本机）
		sendMsg(p, map[string]any{"type": "ctrl", "action": "request-result", "ok": false, "reason": "仅支持控制服务器运行者的屏幕"})
		return
	}
	if act, ok := h.ctrlActive[target]; ok && act.controllerID != p.id {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "request-result", "ok": false, "reason": "该用户已被他人控制"})
		return
	}
	if req, ok := h.ctrlReqs[target]; ok {
		if req.controllerID == p.id {
			sendMsg(p, map[string]any{"type": "ctrl", "action": "request-result", "ok": true, "pending": true})
		} else {
			sendMsg(p, map[string]any{"type": "ctrl", "action": "request-result", "ok": false, "reason": "该用户有正在等待同意的控制请求"})
		}
		return
	}
	name := p.name
	h.ctrlReqs[target] = &ctrlReq{controllerID: p.id, controllerName: name, at: time.Now()}
	sendMsg(tgt, map[string]any{"type": "ctrl", "action": "request", "controller": p.id, "controllerName": name})
}

func (h *hub) ctrlRespond(p *peer, target string, accept bool) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	if p.id != target {
		return // 仅被控端本人可回应
	}
	req := h.ctrlReqs[target]
	if req == nil {
		return
	}
	delete(h.ctrlReqs, target)
	ctl := h.peers[req.controllerID]
	if ctl == nil {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "request-result", "ok": false, "reason": "控制者已离线"})
		return
	}
	if !accept {
		sendMsg(ctl, map[string]any{"type": "ctrl", "action": "denied"})
		return
	}
	h.ctrlActive[target] = req
	sendMsg(ctl, map[string]any{"type": "ctrl", "action": "granted", "target": target})
	sendMsg(p, map[string]any{"type": "ctrl", "action": "controlled", "controller": req.controllerID, "controllerName": req.controllerName})
}

// ctrlInvite 被控端（host）邀请指定观看者控制自己的屏幕
func (h *hub) ctrlInvite(p *peer, target string) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	if !p.host {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "invite-failed", "reason": "仅服务器运行者（本机）可发起控制邀请"})
		return
	}
	if target == "" || target == p.id {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "invite-failed", "reason": "邀请对象无效"})
		return
	}
	invitee := h.peers[target]
	if invitee == nil {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "invite-failed", "reason": "对方不在线"})
		return
	}
	if _, ok := h.ctrlActive[p.id]; ok {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "invite-failed", "reason": "你正被他人控制，无法发起邀请"})
		return
	}
	if req, ok := h.ctrlReqs[p.id]; ok {
		if req.controllerID == target {
			sendMsg(p, map[string]any{"type": "ctrl", "action": "invite-failed", "reason": "已向该用户发出邀请，等待回应"})
		} else {
			sendMsg(p, map[string]any{"type": "ctrl", "action": "invite-failed", "reason": "有正在等待回应的控制请求"})
		}
		return
	}
	h.ctrlReqs[p.id] = &ctrlReq{controllerID: target, controllerName: invitee.name, at: time.Now()}
	sendMsg(invitee, map[string]any{"type": "ctrl", "action": "invite-received", "controller": p.id, "controllerName": p.name})
}

// ctrlInviteRespond 被邀请者回应控制邀请
func (h *hub) ctrlInviteRespond(p *peer, accept bool) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	var target string
	var req *ctrlReq
	for t, r := range h.ctrlReqs {
		if r.controllerID == p.id {
			target, req = t, r
			break
		}
	}
	if req == nil {
		return
	}
	delete(h.ctrlReqs, target)
	host := h.peers[target]
	if host == nil {
		sendMsg(p, map[string]any{"type": "ctrl", "action": "denied", "reason": "对方已离线"})
		return
	}
	if !accept {
		sendMsg(host, map[string]any{"type": "ctrl", "action": "invite-declined"})
		return
	}
	h.ctrlActive[target] = req
	sendMsg(p, map[string]any{"type": "ctrl", "action": "granted", "target": target})
	sendMsg(host, map[string]any{"type": "ctrl", "action": "controlled", "controller": req.controllerID, "controllerName": req.controllerName})
}

// ctrlRelease 控制者主动释放
func (h *hub) ctrlRelease(p *peer) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	for target, req := range h.ctrlActive {
		if req.controllerID == p.id {
			delete(h.ctrlActive, target)
			if tgt := h.peers[target]; tgt != nil {
				sendMsg(tgt, map[string]any{"type": "ctrl", "action": "released"})
			}
			return
		}
	}
}

// ctrlRevoke 被控端收回控制权
func (h *hub) ctrlRevoke(p *peer) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	req := h.ctrlActive[p.id]
	if req == nil {
		return
	}
	delete(h.ctrlActive, p.id)
	if ctl := h.peers[req.controllerID]; ctl != nil {
		sendMsg(ctl, map[string]any{"type": "ctrl", "action": "revoked"})
	}
}

// ctrlIsController 判断 p 是否为某活跃会话的控制者（非本节点 host 无法被控）
func (h *hub) ctrlIsController(p *peer) bool {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	for target, req := range h.ctrlActive {
		if req.controllerID == p.id {
			if h.peers[target] == nil {
				delete(h.ctrlActive, target)
				return false
			}
			return true
		}
	}
	return false
}

// ctrlTargetOf 返回 p 作为控制者的被控端 id（非控制者返回空）
func (h *hub) ctrlTargetOf(p *peer) string {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	for target, req := range h.ctrlActive {
		if req.controllerID == p.id {
			return target
		}
	}
	return ""
}

// ctrlAbsCoords 把主控端相对坐标(0-65535，相对被控端共享的屏幕)换算为虚拟桌面物理像素坐标。
// 依据：被控端共享流分辨率（share-config 上报）匹配显示器布局，得到该屏在虚拟桌面中的位置。
// 单屏/无目标/匹配失败均回退主屏换算（主屏 left/top 通常为 0，行为与原主屏绝对坐标一致）。
func (h *hub) ctrlAbsCoords(p *peer, relX, relY int) (int, int) {
	layout := currentMonitorLayout()
	if len(layout.Monitors) == 0 {
		return relX, relY // 极异常：无任何显示器
	}
	primary := layout.primaryMonitor()
	target := h.ctrlTargetOf(p)
	if target == "" || len(layout.Monitors) <= 1 {
		return layout.toVirtualPx(primary, relX, relY) // 单屏：按主屏换算
	}
	w, ht := 0, 0
	h.mu.Lock()
	if cfg, ok := h.streamCfgs[target]; ok {
		if fw, ok := cfg["width"].(float64); ok {
			w = int(fw)
		}
		if fh, ok := cfg["height"].(float64); ok {
			ht = int(fh)
		}
	}
	h.mu.Unlock()
	if w > 0 && ht > 0 {
		if m, ok := layout.matchMonitor(w, ht); ok {
			return layout.toVirtualPx(m, relX, relY) // 命中副屏：按该屏换算
		}
	}
	return layout.toVirtualPx(primary, relX, relY) // 匹配失败：回退主屏
}

// ctrlCleanup 用户断开时清理其相关控制会话（作为被控端/控制者/待确认请求）
func (h *hub) ctrlCleanup(id string) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	if req, ok := h.ctrlActive[id]; ok { // 作为被控端断开
		delete(h.ctrlActive, id)
		if ctl := h.peers[req.controllerID]; ctl != nil {
			sendMsg(ctl, map[string]any{"type": "ctrl", "action": "revoked", "reason": "对方已断开"})
		}
	}
	for target, req := range h.ctrlActive { // 作为控制者断开
		if req.controllerID == id {
			delete(h.ctrlActive, target)
			if tgt := h.peers[target]; tgt != nil {
				sendMsg(tgt, map[string]any{"type": "ctrl", "action": "released", "reason": "控制者已断开"})
			}
		}
	}
	for target, req := range h.ctrlReqs { // 待确认请求清理
		if req.controllerID == id || target == id {
			delete(h.ctrlReqs, target)
			if tgt := h.peers[target]; tgt != nil && tgt.id != id {
				sendMsg(tgt, map[string]any{"type": "ctrl", "action": "request-cancel"})
			}
		}
	}
}

func (h *hub) broadcast(m map[string]any) {
	h.mu.Lock()
	peers := make([]*peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	for _, p := range peers {
		sendMsg(p, m)
	}
}

// broadcastExcept 向除 except 外的所有 peer 广播
func (h *hub) broadcastExcept(except *peer, m map[string]any) {
	h.mu.Lock()
	peers := make([]*peer, 0, len(h.peers))
	for _, p := range h.peers {
		if p != except {
			peers = append(peers, p)
		}
	}
	h.mu.Unlock()
	for _, p := range peers {
		sendMsg(p, m)
	}
}

func (h *hub) peerInfo(p *peer) map[string]any {
	return map[string]any{"id": p.id, "name": p.name, "color": p.color, "sharing": p.sharing(), "host": p.host, "ip": p.ip, "enterHost": p.enterHost, "hostId": p.hostId, "joinedAt": p.joinedAt}
}

func (p *peer) handleHello(m map[string]any) {
	h := p.hub
	h.mu.Lock()
	name, _ := m["name"].(string)
	clientID, _ := m["clientId"].(string)
	enterHost, _ := m["enterHost"].(string)
	if name == "" {
		name = fmt.Sprintf("访客-%d", h.seq+1)
	}
	if clientID != "" && h.clientIDs[clientID] != nil && h.clientIDs[clientID] != p {
		// 同一浏览器刷新/重连：顶替旧连接，沿用身份（id/颜色）
		old := h.clientIDs[clientID]
		old.replaced = true
		p.clientID = clientID
		p.id = old.id
		p.color = old.color
		p.joinedAt = old.joinedAt // 刷新/重连沿用原在线时长（不算重新加入）
		h.peers[p.id] = p
		h.clientIDs[clientID] = p
		h.mu.Unlock()
		// 清理旧连接资源（推流/订阅/流），避免与新连接冲突
		h.sfu.removeAllStreams(old.id)
		h.sfu.removeViewer(old.id)
		old.mu.Lock()
		pub := old.pubPC
		old.pubPC = nil
		cam := old.camPC
		old.camPC = nil
		audioPubs := old.audioPubPCs
		old.audioPubPCs = nil
		old.mu.Unlock()
		if pub != nil {
			pub.Close()
		}
		if cam != nil {
			cam.Close()
		}
		for _, ap := range audioPubs {
			ap.Close()
		}
		old.conn.Close() // 触发旧连接 readLoop 退出 → unregister（replaced 跳过广播）
		h.mu.Lock()
	} else if _, exists := h.peers[p.id]; !exists {
		h.seq++
		p.id = fmt.Sprintf("u%d", h.seq)
		p.color = palette[(h.seq-1)%len(palette)]
		h.peers[p.id] = p
		if clientID != "" {
			p.clientID = clientID
			h.clientIDs[clientID] = p
		}
	}
	p.name = name           // 重复 hello（设置昵称）也更新名字
	p.enterHost = enterHost // 重复 hello 也更新进入地址（页面刷新/换地址进入）
	// 所属节点服务运行者标记：host 浏览器指向自己；普通用户指向本节点 host（host 未上线为空，上线时补填）
	hostUpdated := false
	if p.host {
		if p.hostId == "" {
			p.hostId = p.id
			hostUpdated = true
		}
		for _, q := range h.peers {
			if q.host {
				continue
			}
			if q.hostId == "" {
				q.hostId = p.id
				hostUpdated = true
			} else if h.peers[q.hostId] == nil {
				// host 重连换了新 id（刷新后顶替失败）：把指向已离线 host 的用户重指到当前 host
				q.hostId = p.id
				hostUpdated = true
			}
		}
	} else if p.hostId == "" {
		for _, q := range h.peers {
			if q.host {
				p.hostId = q.id
				break
			}
		}
	}
	log.Printf("hello: %s(%s) 在线用户=%d 流=%d", p.id, p.name, len(h.peers), len(h.sfu.streams))
	peers := make([]map[string]any, 0, len(h.peers))
	for _, q := range h.peers {
		if q == p {
			continue
		}
		peers = append(peers, h.peerInfo(q))
	}
	h.mu.Unlock()

	streamCfgs := make(map[string]map[string]any, len(h.streamCfgs))
	h.mu.Lock()
	for k, v := range h.streamCfgs {
		streamCfgs[k] = v
	}
	h.mu.Unlock()

	streams := make([]map[string]any, 0, len(h.sfu.streams))
	h.sfu.mu.Lock()
	for _, st := range h.sfu.streams {
		s := map[string]any{"id": st.ID, "ownerId": st.OwnerID, "codec": st.Codec.MimeType}
		if len(st.Layers) == 0 {
			s["audioOnly"] = true
		}
		if cfg, ok := streamCfgs[st.ID]; ok {
			s["config"] = cfg
		}
		streams = append(streams, s)
	}
	h.sfu.mu.Unlock()

	sendMsg(p, map[string]any{"type": "welcome", "id": p.id, "name": p.name, "color": p.color, "host": p.host, "ip": p.ip, "peers": peers, "streams": streams})
	h.broadcastExcept(p, map[string]any{"type": "peer-joined", "id": p.id, "name": p.name, "color": p.color, "host": p.host, "ip": p.ip, "enterHost": p.enterHost, "hostId": p.hostId, "joinedAt": p.joinedAt})
	if hostUpdated {
		// host 上线（含晚于他人加入）：广播补填，前端将 hostId 为空的用户指向该服务运行者
		h.broadcastExcept(p, map[string]any{"type": "host-updated", "id": p.id, "name": p.name})
	}
	h.sfu.ensureSubscriber(p.id)
	// 补发聊天历史（新成员可见最近消息）
	h.mu.Lock()
	hist := make([]map[string]any, len(h.chatHist))
	copy(hist, h.chatHist)
	h.mu.Unlock()
	if len(hist) > 0 {
		sendMsg(p, map[string]any{"type": "chat-history", "messages": hist})
	}
	// 房间人数变化：广播 music-state 刷新"一起听"人数
	h.musicBroadcast()
}

func (p *peer) handlePublishOffer(m map[string]any) {
	h := p.hub
	pcType, _ := m["pc"].(string)
	if pcType == "" {
		pcType = "publish"
	}
	sd, _ := m["sdp"].(map[string]any)
	sdpStr, _ := sd["sdp"].(string)
	sdpType, _ := sd["type"].(string)
	if sdpStr == "" || sdpType != "offer" {
		return
	}

	audioKind := ""
	if pcType == "audio-mic" {
		audioKind = "mic"
	} else if pcType == "audio-system" {
		audioKind = "system"
	}
	// 摄像头流独立 ID（观看端卡片显示为第二视频区）
	streamID := ""
	if pcType == "camera" {
		streamID = p.id + "-cam"
	}

	// 关闭同类型旧连接
	p.mu.Lock()
	if audioKind != "" {
		if p.audioPubPCs[audioKind] != nil {
			p.audioPubPCs[audioKind].Close()
			delete(p.audioPubPCs, audioKind)
		}
	} else if pcType == "camera" {
		if p.camPC != nil {
			p.camPC.Close()
			p.camPC = nil
		}
	} else if pcType == "publish" {
		if p.pubPC != nil {
			p.pubPC.Close()
			p.pubPC = nil
		}
	}
	p.mu.Unlock()

	pc, err := newPeerConnection(h.sfu.iceServers)
	if err != nil {
		return
	}
	if pcType == "publish" {
		// 声明 simulcast 三档接收（l/m/h），保证各层分别触发 OnTrack
		pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{ //nolint:errcheck
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
			SendEncodings: []webrtc.RTPEncodingParameters{
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "l"}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "m"}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "h"}},
			},
		})
	} else if pcType == "camera" {
		// 摄像头流同样声明 simulcast 三档接收（l/m/h），观看端可切换档位
		pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{ //nolint:errcheck
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
			SendEncodings: []webrtc.RTPEncodingParameters{
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "l"}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "m"}},
				{RTPCodingParameters: webrtc.RTPCodingParameters{RID: "h"}},
			},
		})
	} else if audioKind != "" {
		// 音频通道同样预置接收 transceiver（对齐视频路径）：无预置时 pion 自动创建的
		// 传输启动行为与预置不一致，曾导致音频 ICE 卡 checking→failed（视频正常、音频失败）
		pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{ //nolint:errcheck
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
	}
	p.mu.Lock()
	if audioKind != "" {
		if p.audioPubPCs == nil {
			p.audioPubPCs = make(map[string]*webrtc.PeerConnection)
		}
		p.audioPubPCs[audioKind] = pc
	} else if pcType == "camera" {
		p.camPC = pc
	} else {
		p.pubPC = pc
	}
	p.mu.Unlock()

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if remote.Kind() == webrtc.RTPCodecTypeAudio {
			h.onStreamAudio(p.id, remote, pc, audioKind)
			return
		}
		if (pcType == "publish" || pcType == "camera") && remote.Kind() == webrtc.RTPCodecTypeVideo {
			rid := remote.RID()
			if rid == "" {
				rid = "m"
			}
			logV("publish: %s 到达层 rid=%s codec=%s pc=%s", p.id, rid, remote.Codec().MimeType, pcType)
			h.onStreamLayer(p.id, remote, rid, pc, streamID)
		}
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		if pcType == "camera" {
			log.Printf("camera: %s ICE 状态=%s", p.id, s) // 诊断：摄像头推流连接状态
		} else if audioKind != "" {
			log.Printf("audio-%s: %s ICE 状态=%s", audioKind, p.id, s) // 诊断：音频推流连接状态
		} else {
			log.Printf("publish: %s ICE 状态=%s", p.id, s) // 诊断：屏幕推流连接状态（对比音频用）
		}
		// 网络抖动容错：disconnected 是瞬时断流（可能自动恢复），不清理流；
		// 仅 failed/closed 才移除（否则抖动恢复后流已被删，观看/共享中断）
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			if audioKind != "" {
				h.sfu.removeStreamAudio(p.id, audioKind) // 音频通道断开：仅移除该通道（视频流可能仍在）
			} else if pcType == "camera" {
				h.sfu.removeStreamVideo(p.id+"-cam", pc)
			} else {
				h.sfu.removeStreamVideo(p.id, pc) // 屏幕连接断开：仅移除该连接产生的流
			}
		}
	})
	// 候选发送顺序保证：answer 先发、候选后发——浏览器在 setRemoteDescription(answer)
	// 之前 addIceCandidate 会失败且候选被丢弃，导致 ICE 无配对而 failed（曾因日志模式差异 100% 复现）
	var (
		candMu     sync.Mutex
		pendCands  []webrtc.ICECandidateInit
		answerSent bool
	)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cj := c.ToJSON()
		candMu.Lock()
		if !answerSent {
			pendCands = append(pendCands, cj)
			candMu.Unlock()
			return
		}
		candMu.Unlock()
		logV("publish: %s 候选 %s %s:%d pc=%s", p.id, c.Typ.String(), c.Address, c.Port, pcType) // 诊断：候选是否发出
		sendMsg(p, map[string]any{"type": "candidate", "pc": pcType, "candidate": cj})
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdpStr}); err != nil {
		log.Printf("publish: SetRemoteDescription 失败 %s: %v", p.id, err)
		pc.Close()
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("publish: CreateAnswer 失败 %s: %v", p.id, err)
		pc.Close()
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("publish: SetLocalDescription 失败 %s: %v", p.id, err)
		pc.Close()
		return
	}
	log.Printf("publish: %s 推流协商完成 (%s)", p.id, pcType)
	sendMsg(p, map[string]any{"type": "publish-answer", "pc": pcType, "sdp": answer})
	// answer 已发，刷新缓存的候选（保证浏览器先 setRemoteDescription 再 addIceCandidate）
	candMu.Lock()
	answerSent = true
	for _, cj := range pendCands {
		sendMsg(p, map[string]any{"type": "candidate", "pc": pcType, "candidate": cj})
	}
	pendCands = nil
	candMu.Unlock()
}

// rewriteTrack 是自定义 TrackLocal：把转发 RTP 包的 SSRC 改写为
// 每个观看者 sender 协商的固定 SSRC。切档位（ReplaceTrack 换层）时
// 观看者看到的 SSRC 保持不变，避免 Chrome 因 SSRC 与 SDP 不符而花屏。
type rewriteTrack struct {
	codec      webrtc.RTPCodecCapability
	id         string
	streamID   string
	rid        string
	sourcePC   *webrtc.PeerConnection // 源推流连接（发 PLI 用）
	sourceSSRC uint32                 // 源该层 SSRC（发 PLI 用）

	mu      sync.Mutex
	writers map[webrtc.SSRC]webrtc.TrackLocalWriter
}

func newRewriteTrack(c webrtc.RTPCodecCapability, id, streamID, rid string, sourcePC *webrtc.PeerConnection, sourceSSRC uint32) *rewriteTrack {
	return &rewriteTrack{codec: c, id: id, streamID: streamID, rid: rid, sourcePC: sourcePC, sourceSSRC: sourceSSRC, writers: make(map[webrtc.SSRC]webrtc.TrackLocalWriter)}
}

func (t *rewriteTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	t.mu.Lock()
	t.writers[ctx.SSRC()] = ctx.WriteStream()
	t.mu.Unlock()
	// 绑定（含 ReplaceTrack 换层）成功即请求源出关键帧，避免观看者等待自动关键帧而卡住
	t.requestKeyFrame()
	for _, cp := range ctx.CodecParameters() {
		if cp.MimeType == t.codec.MimeType {
			return cp, nil
		}
	}
	// 降级：取任意 video codec，避免协商失败导致 AddTrack/ReplaceTrack 失败
	for _, cp := range ctx.CodecParameters() {
		if strings.HasPrefix(cp.MimeType, "video/") {
			return cp, nil
		}
	}
	return webrtc.RTPCodecParameters{}, fmt.Errorf("no video codec for %s", t.codec.MimeType)
}

func (t *rewriteTrack) requestKeyFrame() {
	if t.sourcePC == nil || t.sourceSSRC == 0 {
		return
	}
	go func() {
		if t.sourcePC.ConnectionState() == webrtc.PeerConnectionStateConnected {
			t.sourcePC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: t.sourceSSRC}}) //nolint:errcheck
			time.Sleep(400 * time.Millisecond)
			if t.sourcePC.ConnectionState() == webrtc.PeerConnectionStateConnected {
				t.sourcePC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: t.sourceSSRC}}) //nolint:errcheck
			}
		}
	}()
}

func (t *rewriteTrack) Unbind(ctx webrtc.TrackLocalContext) error {
	t.mu.Lock()
	delete(t.writers, ctx.SSRC())
	t.mu.Unlock()
	return nil
}

func (t *rewriteTrack) ID() string       { return t.id }
func (t *rewriteTrack) StreamID() string { return t.streamID }
func (t *rewriteTrack) RID() string      { return t.rid }
func (t *rewriteTrack) Kind() webrtc.RTPCodecType {
	if strings.HasPrefix(t.codec.MimeType, "audio/") {
		return webrtc.RTPCodecTypeAudio
	}
	return webrtc.RTPCodecTypeVideo
}

// WriteRTP 按每个观看者协商的 SSRC 分别改写后写入
// 注意：先快照 writer 再释放锁，避免写入阻塞（TCP 背压）时挂起 Bind/Unbind
func (t *rewriteTrack) WriteRTP(p *rtp.Packet) error {
	t.mu.Lock()
	writers := make(map[webrtc.SSRC]webrtc.TrackLocalWriter, len(t.writers))
	for ssrc, w := range t.writers {
		writers[ssrc] = w
	}
	t.mu.Unlock()
	for ssrc, w := range writers {
		p.Header.SSRC = uint32(ssrc)
		if _, err := w.WriteRTP(&p.Header, p.Payload); err != nil {
			return err
		}
	}
	return nil
}

// onStreamAudio 处理共享者的音频 track（音频恒独立流：key = ownerID+"-a"，多通道可共存）
func (h *hub) onStreamAudio(ownerID string, remote *webrtc.TrackRemote, sourcePC *webrtc.PeerConnection, kind string) {
	if kind == "" {
		kind = "audio"
	}
	streamID := ownerID + "-a"
	s := h.sfu
	s.mu.Lock()
	st := s.streams[streamID]
	isNew := false
	if st == nil {
		st = &stream{
			ID:       streamID,
			OwnerID:  ownerID,
			SourcePC: sourcePC,
			Codec:    remote.Codec().RTPCodecCapability,
			Layers:   make(map[string]*rewriteTrack),
			Remote:   make(map[string]*webrtc.TrackRemote),
		}
		s.streams[streamID] = st
		isNew = true
	}
	if st.AudioTracks == nil {
		st.AudioTracks = make(map[string]*rewriteTrack)
		st.AudioRemotes = make(map[string]*webrtc.TrackRemote)
	}
	// 每通道独立 MediaStream（msid = streamID+"-"+kind，如 u3-a-mic）——接收端可分别控制
	audioLocal := newRewriteTrack(remote.Codec().RTPCodecCapability, "audio-"+kind, streamID+"-"+kind, "", sourcePC, uint32(remote.SSRC()))
	st.AudioTracks[kind] = audioLocal
	st.AudioRemotes[kind] = remote
	s.mu.Unlock()

	go func() {
		for {
			pkt, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			if err := audioLocal.WriteRTP(pkt); err != nil {
				return
			}
		}
	}()
	if isNew {
		h.mu.Lock()
		oname := ""
		if owner := h.peers[ownerID]; owner != nil {
			oname = owner.name
		}
		h.mu.Unlock()
		log.Printf("stream-added: %s(%s) 音频流广播", streamID, oname)
		h.broadcast(map[string]any{"type": "stream-added", "id": streamID, "ownerId": ownerID, "ownerName": oname, "codec": remote.Codec().MimeType, "audioOnly": true})
	}
	// 通知已订阅观看者补加音频
	s.ensureAllSubscribers()
	logV("publish: %s 音频到达 kind=%s codec=%s", ownerID, kind, remote.Codec().MimeType)
}

// onStreamLayer 处理共享者的一个 simulcast 层到达，返回是否新流。
// streamID 为空时默认使用 ownerID（屏幕共享）；摄像头流传入 ownerID+"-cam"
func (h *hub) onStreamLayer(ownerID string, remote *webrtc.TrackRemote, rid string, sourcePC *webrtc.PeerConnection, streamID string) bool {
	if streamID == "" {
		streamID = ownerID
	}
	s := h.sfu
	s.mu.Lock()
	st := s.streams[streamID]
	if st == nil {
		st = &stream{
			ID:       streamID,
			OwnerID:  ownerID,
			SourcePC: sourcePC,
			Codec:    remote.Codec().RTPCodecCapability,
			Layers:   make(map[string]*rewriteTrack),
			Remote:   make(map[string]*webrtc.TrackRemote),
		}
		s.streams[streamID] = st
	}
	local := newRewriteTrack(remote.Codec().RTPCodecCapability, "video-"+rid, streamID, rid, sourcePC, uint32(remote.SSRC()))
	st.Layers[rid] = local
	st.Remote[rid] = remote
	isNew := len(st.Layers) == 1
	s.mu.Unlock()

	go func() {
		for {
			pkt, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			if err := local.WriteRTP(pkt); err != nil {
				return
			}
		}
	}()

	if isNew {
		h.mu.Lock()
		oname := ""
		if owner := h.peers[ownerID]; owner != nil {
			oname = owner.name
		}
		h.mu.Unlock()
		log.Printf("stream-added: %s(%s) 新流广播", streamID, oname)
		h.broadcast(map[string]any{"type": "stream-added", "id": streamID, "ownerId": ownerID, "ownerName": oname, "codec": remote.Codec().MimeType})
		go s.streamKeeper(st) // 流保活：REMB（防 Safari 无接收端反馈暂停视频帧）+ 周期性 PLI（关键帧保底）
	}
	// 每次层到达都补订：覆盖"流条目存在但层未就绪时观看者被跳过"的竞态（首次页面/刷新时黑屏隐患）
	s.ensureAllSubscribers()
	return isNew
}

// broadcastWatchers 广播某共享者当前的观看者数量（s.mu 需已释放）
func (s *SFU) broadcastWatchers(ownerID string) {
	s.mu.Lock()
	count := 0
	audioPrefix := ownerID + "-a-"
	for _, vc := range s.viewers {
		subscribed := false
		if _, ok := vc.senders[ownerID]; ok {
			subscribed = true
		}
		if !subscribed {
			for key := range vc.audioSender {
				if len(key) > len(audioPrefix) && key[:len(audioPrefix)] == audioPrefix {
					subscribed = true
					break
				}
			}
		}
		if subscribed {
			count++
		}
	}
	s.mu.Unlock()
	s.hub.broadcast(map[string]any{"type": "watchers", "ownerId": ownerID, "count": count})
}

// ensureSubscriber 确保观看者有拉流连接并包含全部（除自己）流
func (s *SFU) ensureSubscriber(viewerID string) {
	s.mu.Lock()
	vc := s.viewers[viewerID]
	if vc == nil {
		h := s.hub
		var peer *peer
		h.mu.Lock()
		peer = h.peers[viewerID]
		h.mu.Unlock()
		if peer == nil {
			s.mu.Unlock()
			log.Printf("subscribe: %s 不在线，跳过", viewerID)
			return
		}
		pc, err := newPeerConnection(s.iceServers)
		if err != nil {
			s.mu.Unlock()
			log.Printf("subscribe: %s 创建 pc 失败: %v", viewerID, err)
			return
		}
		vc = &viewerConn{peer: peer, pc: pc, senders: make(map[string]*webrtc.RTPSender), audioSender: make(map[string]*webrtc.RTPSender), tiers: make(map[string]string), negCh: make(chan struct{}, 1)}
		pc.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}
			sendMsg(peer, map[string]any{"type": "candidate", "pc": "subscribe", "candidate": c.ToJSON()})
		})
		pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
			logV("subscribe: %s ICE 状态=%s", viewerID, st)
			if st == webrtc.ICEConnectionStateClosed {
				pc.Close()
			}
		})
		s.viewers[viewerID] = vc
		logV("subscribe: %s 订阅连接已创建", viewerID)
	}
	changed := false
	for id, st := range s.streams {
		if id == viewerID || strings.HasPrefix(id, viewerID+"-") {
			continue // 不订阅自己（含自己的子流：主视频/摄像头/音频）
		}
		// 订阅层：优先保留观看者已选档位（vc.tiers），否则默认流畅（省带宽）
		tier := vc.tiers[id]
		local := st.Layers[tier]
		if local == nil {
			local = st.Layers["l"]
		}
		if local == nil {
			for _, l := range st.Layers {
				local = l
				break
			}
		}
		if local == nil {
			if len(st.AudioTracks) > 0 {
				continue // 纯音频流：无视频层，由下方音频循环订阅（不打日志避免刷屏）
			}
			log.Printf("subscribe: %s 流 %s 层未就绪", viewerID, id)
			continue // 层尚未到达
		}
		if old, ok := vc.senders[id]; ok {
			// 同流层替换（编码切换/重开共享后代理流重建）：轨对象已变化 → 换新轨，避免收旧 SSRC 黑屏
			if old.Track() == local {
				continue
			}
			vc.pc.RemoveTrack(old) //nolint:errcheck
			old.Stop()
			delete(vc.senders, id)
		}
		sender, err := vc.pc.AddTrack(local)
		if err != nil {
			log.Printf("subscribe: AddTrack 失败 viewer=%s stream=%s: %v", viewerID, id, err)
			continue
		}
		vc.senders[id] = sender
		if tier == "" {
			tier = "l"
		}
		vc.tiers[id] = tier // 保留用户档位（不因其他流变更/重协商重置）
		go s.forwardPLI(vc, id, sender, st)
		changed = true
		logV("subscribe: %s 已订阅流 %s", viewerID, id)
		// 音频：随视频一起订阅全部通道（无 PLI）
		for kind, al := range st.AudioTracks {
			if aOld, ok := vc.audioSender[id+"-"+kind]; ok {
				if aOld.Track() == al {
					continue
				}
				vc.pc.RemoveTrack(aOld) //nolint:errcheck
				aOld.Stop()
				delete(vc.audioSender, id+"-"+kind)
			}
			aSender, err := vc.pc.AddTrack(al)
			if err != nil {
				log.Printf("subscribe: 音频 AddTrack 失败 viewer=%s stream=%s kind=%s: %v", viewerID, id, kind, err)
				continue
			}
			vc.audioSender[id+"-"+kind] = aSender
			logV("subscribe: %s 已订阅流 %s 音频(%s)", viewerID, id, kind)
		}
	}
	// 音频：视频流后到/纯音频流（无视频层）时订阅全部音频通道（mic/system 可共存）
	for id, st := range s.streams {
		if id == viewerID || strings.HasPrefix(id, viewerID+"-") {
			continue // 不订阅自己（含自己的子流）
		}
		if len(st.AudioTracks) == 0 {
			continue
		}
		if len(st.Layers) > 0 {
			if _, ok := vc.senders[id]; !ok {
				continue // 有视频的流：先订阅视频
			}
		}
		for kind, al := range st.AudioTracks {
			if aOld, ok := vc.audioSender[id+"-"+kind]; ok {
				if aOld.Track() == al {
					continue
				}
				vc.pc.RemoveTrack(aOld) //nolint:errcheck
				aOld.Stop()
				delete(vc.audioSender, id+"-"+kind)
			}
			aSender, err := vc.pc.AddTrack(al)
			if err != nil {
				log.Printf("subscribe: 音频 AddTrack 失败 viewer=%s stream=%s kind=%s: %v", viewerID, id, kind, err)
				continue
			}
			vc.audioSender[id+"-"+kind] = aSender
			changed = true
			logV("subscribe: %s 订阅流 %s 音频(%s)", viewerID, id, kind)
		}
	}
	s.mu.Unlock()
	if changed {
		vc.negotiate()
		// 通知所有共享者观看人数变化
		for id := range s.streams {
			if id != viewerID {
				s.broadcastWatchers(id)
			}
		}
	}
}

func (s *SFU) ensureAllSubscribers() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.viewers))
	for id := range s.viewers {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.ensureSubscriber(id)
	}
}

// resetSubscriber 订阅连接 ICE 失败时重建（前端 resubscribe 触发；P2P 会话独立不受影响）
func (s *SFU) resetSubscriber(viewerID string) {
	s.mu.Lock()
	vc := s.viewers[viewerID]
	if vc == nil {
		s.mu.Unlock()
		return
	}
	if vc.pc != nil {
		vc.pc.Close()
	}
	delete(s.viewers, viewerID)
	s.mu.Unlock()
	s.ensureSubscriber(viewerID)
}

// forwardPLI 把观看者发来的关键帧请求转发给共享者（按当前档位的 SSRC 改写）
func (s *SFU) forwardPLI(vc *viewerConn, streamID string, sender *webrtc.RTPSender, st *stream) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		s.mu.Lock()
		rid := vc.tiers[streamID]
		remote := st.Remote[rid]
		srcPC := st.SourcePC
		var ssrc uint32
		if remote != nil {
			ssrc = uint32(remote.SSRC())
		}
		s.mu.Unlock()
		if remote == nil || srcPC == nil {
			return
		}
		for _, pkt := range pkts {
			if pli, ok := pkt.(*rtcp.PictureLossIndication); ok {
				pli.MediaSSRC = ssrc
				if srcPC.ConnectionState() == webrtc.PeerConnectionStateConnected {
					srcPC.WriteRTCP([]rtcp.Packet{pli}) //nolint:errcheck
				}
			}
		}
	}
}

// streamKeeper 流保活：每秒向源发 REMB（Safari 收不到接收端带宽反馈时会暂停视频帧，音频不受影响），
// 每 2 秒发一次 PLI（关键帧保底，覆盖推流端偶发漏发关键帧的场景）。流移除或源连接断开即退出。
func (s *SFU) streamKeeper(st *stream) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	for range ticker.C {
		s.mu.Lock()
		cur := s.streams[st.ID]
		if cur == nil || cur.SourcePC == nil || cur.SourcePC.ConnectionState() != webrtc.PeerConnectionStateConnected || len(cur.Remote) == 0 {
			s.mu.Unlock()
			return
		}
		ssrcs := make([]uint32, 0, len(cur.Remote))
		var first uint32
		for _, r := range cur.Remote {
			ssrc := uint32(r.SSRC())
			ssrcs = append(ssrcs, ssrc)
			if first == 0 {
				first = ssrc
			}
		}
		pc := cur.SourcePC
		s.mu.Unlock()
		pc.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: 30_000_000, SSRCs: ssrcs}}) //nolint:errcheck
		ticks++
		if ticks%2 == 0 {
			pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: first}}) //nolint:errcheck
		}
	}
}

// negotiate 服务器侧发起（重）协商，串行保证一次一个 offer；
// 有在途 offer（have-local-offer）时标记待办，answer 完成后补发，避免洪泛
func (vc *viewerConn) negotiate() {
	select {
	case vc.negCh <- struct{}{}:
	default:
		return
	}
	defer func() { <-vc.negCh }()

	if vc.pc.SignalingState() != webrtc.SignalingStateStable {
		vc.pmu.Lock()
		vc.pending = true
		vc.pmu.Unlock()
		return
	}
	vc.pmu.Lock()
	vc.pending = false
	vc.pmu.Unlock()

	offer, err := vc.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("negotiate: CreateOffer 失败 %s: %v", vc.peer.id, err)
		return
	}
	if err := vc.pc.SetLocalDescription(offer); err != nil {
		log.Printf("negotiate: SetLocalDescription 失败 %s: %v", vc.peer.id, err)
		return
	}
	// 附带流映射（id -> SSRC）供前端统计按 SSRC 匹配
	streams := make([]map[string]any, 0, len(vc.senders))
	for sid, sender := range vc.senders {
		ssrc := uint32(0)
		if params := sender.GetParameters(); len(params.Encodings) > 0 {
			ssrc = uint32(params.Encodings[0].SSRC)
		}
		streams = append(streams, map[string]any{"id": sid, "ssrc": ssrc})
	}
	logV("negotiate: 发送 subscribe-offer 给 %s (流 %d)", vc.peer.id, len(streams))
	sendMsg(vc.peer, map[string]any{"type": "subscribe-offer", "sdp": vc.pc.LocalDescription(), "streams": streams})
}

func (p *peer) handleSubscribeAnswer(m map[string]any) {
	vc := p.hub.sfu.viewers[p.id]
	if vc == nil {
		return
	}
	sd, _ := m["sdp"].(map[string]any)
	sdpStr, _ := sd["sdp"].(string)
	if sdpStr == "" {
		return
	}
	vc.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdpStr}) //nolint:errcheck
	// answer 完成后补发积压的协商请求（新流/切层等）
	vc.pmu.Lock()
	pending := vc.pending
	vc.pending = false
	vc.pmu.Unlock()
	if pending && vc.pc.SignalingState() == webrtc.SignalingStateStable {
		go vc.negotiate()
	}
}

func (p *peer) handleCandidate(m map[string]any) {
	pcType, _ := m["pc"].(string)
	cand, _ := m["candidate"].(map[string]any)
	if cand == nil {
		return
	}
	c := webrtc.ICECandidateInit{}
	if v, ok := cand["candidate"].(string); ok {
		c.Candidate = v
	}
	if v, ok := cand["sdpMid"].(string); ok {
		c.SDPMid = &v
	}
	if v, ok := cand["sdpMLineIndex"].(float64); ok {
		i := uint16(v)
		c.SDPMLineIndex = &i
	}
	switch pcType {
	case "publish":
		p.mu.Lock()
		pc := p.pubPC
		p.mu.Unlock()
		if pc != nil {
			if err := pc.AddICECandidate(c); err != nil {
				logV("publish: %s 候选 add 失败: %v", p.id, err)
			}
		}
	case "camera":
		p.mu.Lock()
		pc := p.camPC
		p.mu.Unlock()
		if pc != nil {
			if err := pc.AddICECandidate(c); err != nil {
				logV("publish: %s 候选 add 失败: %v", p.id, err)
			}
		}
	case "audio", "audio-mic", "audio-system":
		kind := "audio"
		if pcType == "audio-mic" {
			kind = "mic"
		} else if pcType == "audio-system" {
			kind = "system"
		}
		p.mu.Lock()
		pc := p.audioPubPCs[kind]
		p.mu.Unlock()
		if pc != nil {
			if err := pc.AddICECandidate(c); err != nil {
				logV("publish: %s 候选 add 失败 pc=%s: %v", p.id, pcType, err)
			}
		}
	case "subscribe":
		vc := p.hub.sfu.viewers[p.id]
		if vc != nil {
			vc.pc.AddICECandidate(c) //nolint:errcheck
		}
	}
}

// handleShareConfig 记录并广播共享者的编码配置（供接收端判断解码受限）
func (p *peer) handleShareConfig(m map[string]any) {
	h := p.hub
	h.sfu.mu.Lock()
	_, hasStream := h.sfu.streams[p.id]
	h.sfu.mu.Unlock()
	if !hasStream {
		return
	}
	cfg := map[string]any{
		"frameRate": m["frameRate"],
		"bitrate":   m["bitrate"],
		"width":     m["width"],
		"height":    m["height"],
	}
	h.mu.Lock()
	if h.streamCfgs == nil {
		h.streamCfgs = make(map[string]map[string]any)
	}
	h.streamCfgs[p.id] = cfg
	h.mu.Unlock()
	h.broadcast(map[string]any{
		"type": "share-config", "ownerId": p.id,
		"frameRate": cfg["frameRate"], "bitrate": cfg["bitrate"],
		"width": cfg["width"], "height": cfg["height"],
	})
}

func (p *peer) handleViewerConfig(m map[string]any) {
	streamID, _ := m["streamId"].(string)
	tier, _ := m["tier"].(string)
	s := p.hub.sfu

	s.mu.Lock()
	vc := s.viewers[p.id]
	st := s.streams[streamID]
	if vc == nil || st == nil {
		s.mu.Unlock()
		return
	}
	newLocal := st.Layers[tier]
	if newLocal == nil {
		newLocal = st.Layers["l"] // 目标层未就绪时回退默认档位（流畅）
	}
	oldSender := vc.senders[streamID]
	if oldSender == nil || newLocal == nil {
		s.mu.Unlock()
		return
	}
	// 重协商切层：移除旧层 + 添加新层。新层随协商获得新 SSRC（写入 SDP），
	// Chrome 将其视为新流正常解码，避免同 SSRC 时间戳跳变导致的持续丢包。
	if err := vc.pc.RemoveTrack(oldSender); err != nil {
		log.Printf("viewer-config: RemoveTrack 失败 viewer=%s stream=%s: %v", p.id, streamID, err)
		s.mu.Unlock()
		return
	}
	oldSender.Stop()
	newSender, err := vc.pc.AddTrack(newLocal)
	if err != nil {
		log.Printf("viewer-config: AddTrack 失败 viewer=%s stream=%s tier=%s: %v", p.id, streamID, tier, err)
		s.mu.Unlock()
		return
	}
	vc.senders[streamID] = newSender
	vc.tiers[streamID] = tier
	go s.forwardPLI(vc, streamID, newSender, st)
	remote := st.Remote[tier]
	srcPC := st.SourcePC
	var ssrc uint32
	if remote != nil {
		ssrc = uint32(remote.SSRC())
	}
	s.mu.Unlock()

	// 向源请求关键帧：重协商完成后新层关键帧已就绪，画面更快恢复
	if remote != nil && srcPC != nil && srcPC.ConnectionState() == webrtc.PeerConnectionStateConnected {
		srcPC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}) //nolint:errcheck
	}
	vc.negotiate()
}

// loadChatConfig 加载聊天配置（config/chat.json；缺失用默认值）
func (h *hub) loadChatConfig() {
	h.chatCfg = chatConfig{HistKeep: 100, FileTotal: 200, FileExpire: 60, FileMax: 100, BigThreshold: 20, MsgLimit: 2000, Persist: true}
	data, err := os.ReadFile("config/chat.json")
	if err != nil {
		return
	}
	json.Unmarshal(data, &h.chatCfg)
	if h.chatCfg.HistKeep <= 0 {
		h.chatCfg.HistKeep = 100
	}
	if h.chatCfg.FileTotal <= 0 {
		h.chatCfg.FileTotal = 200
	}
	if h.chatCfg.FileExpire <= 0 {
		h.chatCfg.FileExpire = 60
	}
	if h.chatCfg.FileMax <= 0 {
		h.chatCfg.FileMax = 100
	}
	if h.chatCfg.BigThreshold <= 0 {
		h.chatCfg.BigThreshold = 20
	}
	if h.chatCfg.MsgLimit <= 0 {
		h.chatCfg.MsgLimit = 2000
	}
	// 持久化开启：打开 chat.jsonl 追加写
	if h.chatCfg.Persist {
		_ = os.MkdirAll("config", 0o755)
		f, err := os.OpenFile("config/chat.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			h.chatLog = f
		}
	}
}

// saveChatConfig 保存聊天配置（config/chat.json）
func (h *hub) saveChatConfig() error {
	if err := os.MkdirAll("config", 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h.chatCfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile("config/chat.json", data, 0o600)
}

// loadChatHist 启动时从 chat.jsonl 尾部加载最近 N 条（重启保留历史）
func (h *hub) loadChatHist() {
	data, err := os.ReadFile("config/chat.jsonl")
	if err != nil {
		return
	}
	keep := h.chatCfg.HistKeep
	if keep <= 0 {
		keep = 100
	}
	var lines [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	h.mu.Lock()
	for _, ln := range lines {
		var m map[string]any
		if json.Unmarshal(ln, &m) == nil && m["type"] == "chat" {
			h.chatHist = append(h.chatHist, m)
		}
	}
	h.mu.Unlock()
}

func (p *peer) handleChat(m map[string]any) {
	h := p.hub
	text, _ := m["text"].(string)
	fileId, _ := m["fileId"].(string)
	fileName, _ := m["fileName"].(string)
	fileType, _ := m["fileType"].(string)
	if text == "" && fileId == "" {
		return
	}
	// 按字符数截断（配置上限；[]rune 避免中文按字节截断导致显示少于上限）
	limit := h.chatCfg.MsgLimit
	if limit <= 0 {
		limit = 2000
	}
	runes := []rune(text)
	if len(runes) > limit {
		text = string(runes[:limit])
	}
	msg := map[string]any{
		"type": "chat",
		"from": p.id, "fromName": p.name, "color": p.color,
		"text": text, "ts": time.Now().UnixMilli(),
		"fileId": fileId, "fileName": fileName, "fileType": fileType,
	}
	h.appendChat(msg)
}

// appendChat 本地广播 + 写入聊天历史（含历史截断与文件缓存清理）
func (h *hub) appendChat(msg map[string]any) {
	h.mu.Lock()
	h.chatHist = append(h.chatHist, msg)
	keep := h.chatCfg.HistKeep
	if keep <= 0 {
		keep = 100
	}
	if len(h.chatHist) > keep {
		// 超上限：截断最旧消息，并清理不再被历史引用的文件缓存（小文件随数量上限清理）
		removed := h.chatHist[:len(h.chatHist)-keep]
		h.chatHist = h.chatHist[len(h.chatHist)-keep:]
		for _, rm := range removed {
			fid, _ := rm["fileId"].(string)
			if fid == "" {
				continue
			}
			stillUsed := false
			for _, m2 := range h.chatHist {
				if f2, _ := m2["fileId"].(string); f2 == fid {
					stillUsed = true
					break
				}
			}
			if !stillUsed {
				if cf, ok := h.chatFiles[fid]; ok {
					h.chatTotalB -= int64(len(cf.data))
				}
				delete(h.chatFiles, fid)
			}
		}
	}
	// 持久化：追加写 chat.jsonl（磁盘历史）
	if h.chatCfg.Persist {
		h.chatLogMu.Lock()
		line, err := json.Marshal(msg)
		if err == nil {
			h.chatLog.Write(append(line, '\n'))
		}
		h.chatLogMu.Unlock()
	}
	h.mu.Unlock()
	h.broadcast(msg) // 含发送者：前端统一渲染，不做本地回显
}

// ---------- 聊天文件（内存缓存，重启清空，60 分钟过期清理） ----------
const chatFileTotalMax = 200 << 20 // 默认总文件缓存上限（配置未设时兜底）
// handleChatConfig GET/POST /api/chat-config：查询/保存聊天配置（仅 host；保存后重启生效）
func (h *hub) handleChatConfig(w http.ResponseWriter, r *http.Request) {
	if !isHostConn(r) {
		http.Error(w, "仅主机可操作", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost {
		var body chatConfig
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.HistKeep <= 0 || body.HistKeep > 500 {
			body.HistKeep = 100
		}
		if body.FileTotal <= 0 || body.FileTotal > 1024 {
			body.FileTotal = 200
		}
		if body.FileExpire <= 0 || body.FileExpire > 1440 {
			body.FileExpire = 60
		}
		if body.FileMax <= 0 || body.FileMax > 200 {
			body.FileMax = 100
		}
		if body.BigThreshold <= 0 || body.BigThreshold > 100 {
			body.BigThreshold = 20
		}
		if body.MsgLimit < 500 || body.MsgLimit > 5000 {
			body.MsgLimit = 2000
		}
		h.mu.Lock()
		h.chatCfg = body
		h.mu.Unlock()
		if err := h.saveChatConfig(); err != nil {
			http.Error(w, "保存失败", http.StatusInternalServerError)
			return
		}
	}
	h.mu.Lock()
	cfg := h.chatCfg
	h.mu.Unlock()
	json.NewEncoder(w).Encode(cfg)
}

// handleChatFileUpload POST /api/chat-file：multipart 字段 file，返回 {id, mime, name}
func (h *hub) handleChatFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fileMax := h.chatCfg.FileMax
	if fileMax <= 0 {
		fileMax = 100
	}
	bigThr := h.chatCfg.BigThreshold
	if bigThr <= 0 {
		bigThr = 20
	}
	fileMaxB := int64(fileMax) << 20
	bigThrB := int64(bigThr) << 20
	r.Body = http.MaxBytesReader(w, r.Body, fileMaxB+1<<20)
	if err := r.ParseMultipartForm(fileMaxB); err != nil {
		http.Error(w, fmt.Sprintf("文件过大或解析失败（≤%dMB）", fileMax), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file 字段", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 || int64(len(data)) > fileMaxB {
		http.Error(w, fmt.Sprintf("文件读取失败或超过 %dMB", fileMax), http.StatusBadRequest)
		return
	}
	id := randomCred(8)
	big := int64(len(data)) > bigThrB
	cf := &chatFile{mime: header.Header.Get("Content-Type"), name: header.Filename, at: time.Now(), big: big}
	// 大文件落盘（减少内存占用）；小文件内存缓存
	if big {
		if err := os.MkdirAll("config/chatfiles", 0o755); err == nil {
			path := filepath.Join("config", "chatfiles", id)
			if err := os.WriteFile(path, data, 0o600); err == nil {
				cf.path = path
				cf.data = nil // 释放内存
			}
		}
	}
	if cf.path == "" {
		cf.data = data // 落盘失败或小文件：内存缓存
	}
	fileTotalMax := int64(h.chatCfg.FileTotal) << 20 // 配置总上限（MB）
	if fileTotalMax <= 0 {
		fileTotalMax = chatFileTotalMax
	}
	h.mu.Lock()
	h.chatFiles[id] = cf
	h.chatTotalB += int64(len(cf.data))
	// 总缓存超上限：按最旧优先删除（保留新文件），直到放下
	need := h.chatTotalB
	if need > fileTotalMax {
		type item struct {
			id string
			at time.Time
		}
		var items []item
		for fid, c2 := range h.chatFiles {
			items = append(items, item{fid, c2.at})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) }) // 最旧在前
		for _, it := range items {
			if it.id == id {
				continue // 保留新文件
			}
			del := h.chatFiles[it.id]
			sz := int64(len(del.data))
			delete(h.chatFiles, it.id)
			h.chatTotalB -= sz
			need -= sz
			if del.path != "" {
				os.Remove(del.path) // 删除磁盘文件
			}
			log.Printf("chat-file: 缓存超限，清理最旧 %s（%d 字节）", it.id, sz)
			if need <= fileTotalMax {
				break
			}
		}
	}
	h.mu.Unlock()
	log.Printf("chat-file: %s 上传 %s (%d 字节%s)", pname(r), header.Filename, len(data), map[bool]string{true: ", 已落盘", false: ""}[big])
	json.NewEncoder(w).Encode(map[string]any{"id": id, "mime": header.Header.Get("Content-Type"), "name": header.Filename})
}

func pname(r *http.Request) string {
	if hstr, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return hstr
	}
	return r.RemoteAddr
}

// handleChatFileGet GET /api/chat-file/{id}
func (h *hub) handleChatFileGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/chat-file/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	cf := h.chatFiles[id]
	h.mu.Unlock()
	if cf == nil {
		http.Error(w, "文件不存在或已过期", http.StatusNotFound)
		return
	}
	cd := `inline` // 图片/视频内联预览播放；其余附件下载
	if !strings.HasPrefix(cf.mime, "image/") && !strings.HasPrefix(cf.mime, "video/") {
		cd = `attachment`
	}
	w.Header().Set("Content-Type", cf.mime)
	w.Header().Set("Content-Disposition", cd+`; filename="`+strings.ReplaceAll(cf.name, `"`, `_`)+`"`)
	if cf.path != "" {
		// 大文件落盘：从磁盘读取（不占内存）
		data, err := os.ReadFile(cf.path)
		if err != nil {
			http.Error(w, "文件读取失败", http.StatusNotFound)
			return
		}
		w.Write(data)
		return
	}
	w.Write(cf.data)
}

// chatFileCleanup 定时清理过期的大文件（配置分钟）；小文件随聊天历史上限截断清理
func (h *hub) chatFileCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		expire := h.chatCfg.FileExpire
		if expire <= 0 {
			expire = 60
		}
		cutoff := time.Now().Add(-time.Duration(expire) * time.Minute)
		h.mu.Lock()
		for id, cf := range h.chatFiles {
			if cf.big && cf.at.Before(cutoff) {
				h.chatTotalB -= int64(len(cf.data))
				delete(h.chatFiles, id)
				if cf.path != "" {
					os.Remove(cf.path) // 删除磁盘文件
				}
			}
		}
		h.mu.Unlock()
	}
}

// ---------- 链接预览（OG 元数据，供聊天链接卡片展示） ----------

// GET /api/link-preview?url=...：抓取目标页 og:title/og:description/og:image（回退 <title>）
func (h *hub) handleLinkPreview(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("url")
	if url == "" || (!strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://")) {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{})
		return
	}
	// 模拟浏览器请求：无 UA 会被 B站等站点拒绝/返回错误页
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{})
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{})
		return
	}
	html := string(body)
	// 通用 meta 解析：property/name 两种属性 + 两种属性顺序 + 多值回退
	meta := func(attr string, values ...string) string {
		for _, v := range values {
			re1 := regexp.MustCompile(`<meta[^>]+` + attr + `=["']` + v + `["'][^>]+content=["']([^"']*)["']`)
			if m := re1.FindStringSubmatch(html); len(m) > 1 {
				return htmlUnescape(m[1])
			}
			re2 := regexp.MustCompile(`<meta[^>]+content=["']([^"']*)["'][^>]+` + attr + `=["']` + v + `["']`)
			if m := re2.FindStringSubmatch(html); len(m) > 1 {
				return htmlUnescape(m[1])
			}
		}
		return ""
	}
	// 标题回退链：og:title → twitter:title → <title>
	title := meta("property", "og:title")
	if title == "" {
		title = meta("name", "og:title", "twitter:title")
	}
	if title == "" {
		re := regexp.MustCompile(`<title[^>]*>([^<]*)</title>`)
		if m := re.FindStringSubmatch(html); len(m) > 1 {
			title = htmlUnescape(m[1])
		}
	}
	// 描述回退链：og:description → name=description（国内站常用） → twitter:description
	desc := meta("property", "og:description")
	if desc == "" {
		desc = meta("name", "og:description", "description", "twitter:description")
	}
	// 缩略图回退链：og:image → twitter:image → <link rel="image_src">
	image := meta("property", "og:image")
	if image == "" {
		image = meta("name", "og:image", "twitter:image")
	}
	if image == "" {
		re := regexp.MustCompile(`<link[^>]+rel=["']image_src["'][^>]+href=["']([^"']*)["']`)
		if m := re.FindStringSubmatch(html); len(m) > 1 {
			image = htmlUnescape(m[1])
		}
	}
	// og:image 相对地址补全协议
	if image != "" && strings.HasPrefix(image, "//") {
		image = "https:" + image
	} else if image != "" && strings.HasPrefix(image, "/") {
		if u, err := neturl.Parse(url); err == nil {
			image = u.Scheme + "://" + u.Host + image
		}
	}
	if len([]rune(title)) > 200 {
		title = string([]rune(title)[:200])
	}
	if len([]rune(desc)) > 400 {
		desc = string([]rune(desc)[:400])
	}
	json.NewEncoder(w).Encode(map[string]any{"title": title, "desc": desc, "image": image})
}

// htmlUnescape 简单反转义（HTML 实体 → 文本）
func htmlUnescape(s string) string {
	repl := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&#x2F;", "/",
	)
	return repl.Replace(s)
}

// removeAllStreams 删除共享者的全部流（视频流 + 独立音频流），并通知所有观看者移除
func (s *SFU) removeAllStreams(ownerID string) {
	// 视频流
	s.removeStreamVideo(ownerID, nil)
	// 摄像头流（ownerID-cam）
	s.removeStreamVideo(ownerID+"-cam", nil)
	// 独立音频流（ownerID-a）
	s.removeAudioStreamAll(ownerID)
}

// removeAudioStreamAll 删除共享者的整个音频流（全部通道）
func (s *SFU) removeAudioStreamAll(ownerID string) {
	streamID := ownerID + "-a"
	s.mu.Lock()
	st := s.streams[streamID]
	if st == nil {
		s.mu.Unlock()
		return
	}
	delete(s.streams, streamID)
	toNegotiate := make([]*viewerConn, 0)
	for _, vc := range s.viewers {
		for key, aSender := range vc.audioSender {
			if len(key) > len(streamID) && key[:len(streamID)+1] == streamID+"-" {
				vc.pc.RemoveTrack(aSender) //nolint:errcheck
				aSender.Stop()
				delete(vc.audioSender, key)
				toNegotiate = append(toNegotiate, vc)
			}
		}
	}
	s.mu.Unlock()
	for _, vc := range toNegotiate {
		vc.negotiate()
	}
	s.hub.broadcast(map[string]any{"type": "stream-removed", "id": streamID, "ownerId": ownerID})
	s.broadcastWatchers(ownerID)
}

// removeStreamVideo 移除共享者的视频流（屏幕共享停止；音频为独立流，不受影响）。
// sourcePC 非空时校验来源连接：流不是该连接产生的则跳过（编码热切换后旧连接断开事件晚于新 offer，
// 避免误删新连接刚建立的流）；sourcePC 为 nil 表示无条件移除（停止共享/断线清理）
func (s *SFU) removeStreamVideo(ownerID string, sourcePC *webrtc.PeerConnection) {
	s.mu.Lock()
	st := s.streams[ownerID]
	if st == nil {
		s.mu.Unlock()
		return
	}
	if sourcePC != nil && st.SourcePC != nil && st.SourcePC != sourcePC {
		s.mu.Unlock()
		return // 不是该连接产生的流：跳过（热切换竞态保护）
	}
	delete(s.streams, ownerID)
	toNegotiate := make([]*viewerConn, 0)
	for _, vc := range s.viewers {
		if sender, ok := vc.senders[ownerID]; ok {
			vc.pc.RemoveTrack(sender) //nolint:errcheck
			sender.Stop()
			delete(vc.senders, ownerID)
			delete(vc.tiers, ownerID)
			toNegotiate = append(toNegotiate, vc)
		}
	}
	srcPC := st.SourcePC
	s.mu.Unlock()
	if srcPC != nil {
		srcPC.Close()
	}
	for _, vc := range toNegotiate {
		vc.negotiate()
	}
	s.hub.broadcast(map[string]any{"type": "stream-removed", "id": ownerID, "ownerId": ownerID})
	s.broadcastWatchers(ownerID)
	log.Printf("removeStreamVideo: %s 视频流移除（音频独立流保留）", ownerID)
}

// removeStreamAudio 移除共享者的音频（仅音频共享时整个流移除；有视频时仅移除音频轨）
func (s *SFU) removeStreamAudio(ownerID string, kind string) {
	if kind == "" {
		kind = "audio"
	}
	streamID := ownerID + "-a"
	s.mu.Lock()
	st := s.streams[streamID]
	if st == nil {
		s.mu.Unlock()
		return
	}
	// 移除指定通道
	if st.AudioTracks != nil {
		delete(st.AudioTracks, kind)
		delete(st.AudioRemotes, kind)
	}
	hasAudio := len(st.AudioTracks) > 0
	if !hasAudio {
		// 无剩余音频通道：删除整个音频流
		delete(s.streams, streamID)
		toNegotiate := make([]*viewerConn, 0)
		for _, vc := range s.viewers {
			for key, aSender := range vc.audioSender {
				if len(key) > len(streamID) && key[:len(streamID)+1] == streamID+"-" {
					vc.pc.RemoveTrack(aSender) //nolint:errcheck
					aSender.Stop()
					delete(vc.audioSender, key)
					toNegotiate = append(toNegotiate, vc)
				}
			}
		}
		s.mu.Unlock()
		for _, vc := range toNegotiate {
			vc.negotiate()
		}
		s.hub.broadcast(map[string]any{"type": "stream-removed", "id": streamID, "ownerId": ownerID})
		s.broadcastWatchers(ownerID)
		return
	}
	// 有剩余通道：仅移除该通道
	toNegotiate := make([]*viewerConn, 0)
	key := streamID + "-" + kind
	for _, vc := range s.viewers {
		if aSender, ok := vc.audioSender[key]; ok {
			vc.pc.RemoveTrack(aSender) //nolint:errcheck
			aSender.Stop()
			delete(vc.audioSender, key)
			toNegotiate = append(toNegotiate, vc)
		}
	}
	s.mu.Unlock()
	for _, vc := range toNegotiate {
		vc.negotiate()
	}
	s.hub.broadcast(map[string]any{"type": "stream-audio-removed", "id": streamID, "ownerId": ownerID, "kind": kind})
}

func (s *SFU) removeViewer(viewerID string) {
	s.mu.Lock()
	vc := s.viewers[viewerID]
	if vc != nil {
		delete(s.viewers, viewerID)
		subscribed := make([]string, 0, len(vc.senders))
		for id := range vc.senders {
			subscribed = append(subscribed, id)
		}
		vc.pc.Close()
		s.mu.Unlock()
		for _, id := range subscribed {
			s.broadcastWatchers(id)
		}
		return
	}
	s.mu.Unlock()
}

func (h *hub) unregister(p *peer) {
	h.mu.Lock()
	if h.peers[p.id] != p {
		// 身份已被顶替或不存在：仅清理自身连接与身份映射
		if p.clientID != "" && h.clientIDs[p.clientID] == p {
			delete(h.clientIDs, p.clientID)
		}
		if !p.closed {
			p.closed = true
			close(p.send)
		}
		h.mu.Unlock()
		return
	}
	delete(h.peers, p.id)
	delete(h.streamCfgs, p.id)
	if p.clientID != "" && h.clientIDs[p.clientID] == p {
		delete(h.clientIDs, p.clientID)
	}
	if !p.closed {
		p.closed = true
		close(p.send)
	}
	p.mu.Lock()
	pub := p.pubPC
	p.pubPC = nil
	cam := p.camPC
	p.camPC = nil
	audioPubs := p.audioPubPCs
	p.audioPubPCs = nil
	p.mu.Unlock()
	h.mu.Unlock()

	h.ctrlCleanup(p.id)      // 远程控制会话清理（被控端/控制者/待确认请求）
	h.logUnsubscribeID(p.id) // 日志窗口订阅清理

	if pub != nil {
		pub.Close()
	}
	if cam != nil {
		cam.Close()
	}
	for _, ap := range audioPubs {
		ap.Close()
	}
	// 被顶替的旧连接：资源已在顶替时清理，跳过 peer-left 广播（避免身份抖动）
	if p.replaced {
		return
	}
	h.sfu.removeAllStreams(p.id)
	h.sfu.removeViewer(p.id)
	h.broadcast(map[string]any{"type": "peer-left", "id": p.id})
	h.musicNoPeerTick() // 无人判定（WS 断开）
	h.musicBroadcast()  // 房间人数变化：刷新"一起听"人数
}

func sendMsg(p *peer, m map[string]any) {
	if p == nil || p.conn == nil { // 无连接通道的 peer 不走浏览器发送
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	select {
	case p.send <- data:
	default:
	}
}
