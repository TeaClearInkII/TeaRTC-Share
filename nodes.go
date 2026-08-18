package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// ---------- 多主机互联（mesh 全互联：任意一主机连接另一主机即可自动互联） ----------

const (
	nodeFileChunkSize = 1 << 20 // 跨节点文件分片 1MB
	nodeTokenTTL      = 2 * time.Minute
	nodeWSReadLimit   = 4 << 20 // 节点 WS 消息上限（含 base64 分片）
)

var nodeDialer = &websocket.Dialer{
	TLSClientConfig:  &tls.Config{InsecureSkipVerify: true}, // 家庭信任域：节点间跳过证书校验
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   32 * 1024,
	WriteBufferSize:  32 * 1024,
}

// nodeLink 一个邻居节点（节点 WS + 节点间 WebRTC 双向全量互转）
type nodeLink struct {
	hub        *hub
	id         string // 远端节点 id（node-hello 后确定）
	remote     string // 远端地址（日志/转发）
	conn       *websocket.Conn
	send       chan []byte
	closed     bool
	initiator  bool   // 本端是连接发起方（负责首次 offer）
	peerToken  string // 对端签发的再互联凭证（mesh 自动扩展用）
	forwardTok string // 本端为对端签发的再互联凭证（转发给其他邻居）
	peerIDs    map[string]bool

	pc      *webrtc.PeerConnection
	sent    map[string]*webrtc.RTPSender // key = streamID|suffix（rid/kind）-> sender
	negCh   chan struct{}
	pmu     sync.Mutex
	pending bool

	rttMs      int64 // 本节点→该节点直连延迟（pmu 保护，节点间 ping 测量）
	handshaked bool  // 已握手：仅首次 hello 回复（防 hello 回环风暴）
	retry      bool  // 允许自动重连（发起方连接断开后指数退避重试；被替换/自连拒绝时置 false）
	subscribed bool  // 对端是否订阅本节点流（按需转发：有观看者才转，pmu 保护）

	rtcpOnce map[*webrtc.RTPReceiver]bool // 每个 RTPReceiver 只起一个 RTCP 读取 goroutine
	ssrcMap  map[uint32]string            // 代理流 SSRC -> 原始流 id（PLI 按 SSRC 定向转发）
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:n*2]
	}
	return hex.EncodeToString(b)
}

// ---------- HTTP 入口 ----------

// POST /api/join {remote}：仅本机可调。向远端请求 join token 后建立节点连接
// POST /api/join {remote}：仅本机可调。自动互联：页面（https://[对方IPv6]:port）加载后
// 直接 POST 本机该接口 {remote: 页面地址}，本机即自动连接对方节点
func (h *hub) handleJoin(w http.ResponseWriter, r *http.Request) {
	// CORS 预检：https 页面跨源请求 http://localhost（loopback 混合内容豁免）；
	// 反射具体 Origin（Chrome 私有网络访问严格模式不接受通配符）
	corsOrigin := r.Header.Get("Origin")
	if corsOrigin == "" {
		corsOrigin = "*"
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", corsOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", corsOrigin)
	if !isHostConn(r) {
		http.Error(w, "仅本机可发起组网", http.StatusForbidden)
		return
	}
	var body struct {
		Remote string `json:"remote"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Remote == "" {
		http.Error(w, "缺少 remote 地址", http.StatusBadRequest)
		return
	}
	remote := body.Remote
	remote = normJoinRemote(remote, r.Host)
	if remote == "" {
		http.Error(w, "地址格式无效", http.StatusBadRequest)
		return
	}
	// 防 CSRF：Origin 必须是 IP/localhost 页面；remote 与 Origin 同主机，
	// 或 Origin 为本机页面（手动互联场景，用户主动输入目标）
	origin := r.Header.Get("Origin")
	if origin != "" {
		rHost := urlHost(remote)
		oHost := urlHost(origin)
		if !isIPOrLocalhost(oHost) {
			http.Error(w, "非法的组网请求来源", http.StatusForbidden)
			return
		}
		if rHost != oHost && !isHostName(oHost) {
			http.Error(w, "非法的组网请求来源", http.StatusForbidden)
			return
		}
	}
	// 目标即本机（本机页面用本机 IPv6 打开自测）：无需互联
	if isHostName(urlHost(remote)) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "already": true})
		return
	}
	// 防重复：已互联该节点则跳过（刷新页面/重复自动触发不抖动重建）
	h.mu.Lock()
	dup := false
	for _, nl := range h.nodes {
		if normHost(nl.remote) == normHost(remote) {
			dup = true
			break
		}
	}
	h.mu.Unlock()
	if dup {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "already": true})
		return
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Post(strings.TrimSuffix(remote, "/")+"/api/join-token", "application/json", nil)
	if err != nil {
		http.Error(w, "目标节点不可达: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var jr struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil || jr.Token == "" {
		http.Error(w, "目标节点未返回令牌", http.StatusBadGateway)
		return
	}
	go h.dialNode(remote, jr.Token, "")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": jr.Token})
}

// isIPOrLocalhost 判断主机名是否为 IP 地址或 localhost
func isIPOrLocalhost(h string) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// isHostName 判断主机名是否为本机（loopback 或本服务监听地址，手动互联场景）
func isHostName(h string) bool {
	if h == "" {
		return false
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return h == "localhost"
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

// urlHost 提取 URL 的主机名（去端口，IPv6 去方括号）
func urlHost(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// normHost 提取主机名（兼容无 scheme 的 IP:port / [IPv6]:port 形式，url.Parse 对无 scheme 会失败）
func normHost(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	if strings.HasPrefix(u, "[") {
		if i := strings.IndexByte(u, ']'); i >= 0 {
			return u[1:i]
		}
		return u
	}
	if i := strings.IndexByte(u, ':'); i >= 0 {
		return u[:i]
	}
	return u
}

// normJoinRemote 手动互联地址归一化：http→https、IPv6 补方括号、无端口补本机端口、去斜杠。
// hostPort 为请求 Host（"IP:port"），用于缺省端口补全
func normJoinRemote(remote, hostPort string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	remote = strings.TrimSuffix(remote, "/")
	if strings.HasPrefix(remote, "http://") {
		remote = "https://" + strings.TrimPrefix(remote, "http://")
	} else if !strings.HasPrefix(remote, "https://") {
		remote = "https://" + remote
	}
	// IPv6 无方括号：先试整串作为合法 IPv6 地址（无端口）；失败再拆最后数字段作端口
	if i := strings.Index(remote, "://"); i >= 0 {
		rest := remote[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		if !strings.HasPrefix(rest, "[") && strings.Count(rest, ":") >= 2 {
			if net.ParseIP(rest) != nil {
				remote = remote[:i+3] + "[" + rest + "]"
			} else {
				parts := strings.Split(rest, ":")
				last := parts[len(parts)-1]
				if n, err := strconv.Atoi(last); err == nil && n > 0 && n < 65536 && len(last) <= 5 {
					joined := strings.Join(parts[:len(parts)-1], ":")
					if net.ParseIP(joined) != nil {
						remote = remote[:i+3] + "[" + joined + "]:" + last
					}
				}
			}
		}
	}
	// 无端口补本机端口（从请求 Host 提取，缺省 7553）
	if u, err := url.Parse(remote); err == nil {
		if u.Port() == "" {
			defPort := "7553"
			if _, p, e := net.SplitHostPort(hostPort); e == nil && p != "" {
				defPort = p
			}
			u.Host = net.JoinHostPort(u.Hostname(), defPort)
			remote = u.String()
		}
		remote = strings.TrimSuffix(remote, "/")
	} else {
		return ""
	}
	return remote
}

// POST /api/join-token：签发一次性 join token（等同"知道地址即可加入"的安全级别）
func (h *hub) handleJoinToken(w http.ResponseWriter, r *http.Request) {
	tok := randomToken(16)
	h.mu.Lock()
	h.joinTokens[tok] = time.Now()
	h.mu.Unlock()
	logV("node: 签发 join token 来源=%s", pname(r))
	json.NewEncoder(w).Encode(map[string]any{"token": tok})
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

// GET /node-ws?token=...：节点 WS 服务端入口（join token 或再互联凭证校验）
func (h *hub) serveNodeWS(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	forward := r.URL.Query().Get("forward") == "1"
	forNode := r.URL.Query().Get("for")
	if tok == "" {
		http.Error(w, "missing token", http.StatusForbidden)
		return
	}
	h.mu.Lock()
	ok := false
	if forward {
		bound, has := h.forwardTokens[tok]
		ok = has && bound == forNode
		if ok {
			delete(h.forwardTokens, tok)
		}
	} else {
		at, has := h.joinTokens[tok]
		ok = has && time.Since(at) < nodeTokenTTL
		if ok {
			delete(h.joinTokens, tok)
		}
	}
	h.mu.Unlock()
	if !ok {
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(nodeWSReadLimit)
	nl := &nodeLink{hub: h, remote: r.RemoteAddr, conn: conn, send: make(chan []byte, 64), peerIDs: make(map[string]bool), sent: make(map[string]*webrtc.RTPSender), rtcpOnce: make(map[*webrtc.RTPReceiver]bool), ssrcMap: make(map[uint32]string)}
	go nl.writeLoop()
	nl.readLoop()
}

// dialNode 主动连接远端节点 /node-ws（join 或 mesh 自动扩展）
func (h *hub) dialNode(remote, token, forNode string) {
	base := strings.TrimSuffix(strings.TrimPrefix(remote, "https://"), "/")
	base = strings.TrimPrefix(base, "http://")
	u := "wss://" + base + "/node-ws?token=" + url.QueryEscape(token)
	if forNode != "" {
		u += "&forward=1&for=" + url.QueryEscape(forNode)
	}
	conn, resp, err := nodeDialer.Dial(u, nil)
	if err != nil {
		log.Printf("node: 连接 %s 失败: %v", remote, err)
		return // linkPending 残留（连接失败不再自动重试，可重新组网/重启恢复）
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	conn.SetReadLimit(nodeWSReadLimit)
	nl := &nodeLink{hub: h, remote: remote, conn: conn, send: make(chan []byte, 64), initiator: true, retry: true, peerIDs: make(map[string]bool), sent: make(map[string]*webrtc.RTPSender), rtcpOnce: make(map[*webrtc.RTPReceiver]bool), ssrcMap: make(map[uint32]string)}
	go nl.writeLoop()
	// 发起方主动发送首包 node-hello（对端 handleHello 回复并交换身份），否则两端都等待对方先发导致互联永不建立
	nl.sendMsg(map[string]any{"type": "node-hello", "nodeId": h.nodeId})
	logV("node: 已连接 %s，等待节点握手", remote)
	nl.readLoop()
}

// ---------- 节点 WS 生命周期 ----------

func (nl *nodeLink) writeLoop() {
	for data := range nl.send {
		if err := nl.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}
}

func (nl *nodeLink) sendMsg(m map[string]any) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	select {
	case nl.send <- data:
	default:
	}
}

func (nl *nodeLink) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("node: readLoop panic（已恢复）: %v\n%s", r, debug.Stack())
		}
		nl.close()
	}()
	for {
		_, data, err := nl.conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		nl.handle(m)
	}
}

func (nl *nodeLink) handle(m map[string]any) {
	switch m["type"] {
	case "node-hello":
		nl.handleHello(m)
	case "node-peers":
		if arr, ok := m["peers"].([]any); ok {
			for _, p := range arr {
				if pm, ok := p.(map[string]any); ok {
					nl.remotePeerAdd(pm)
				}
			}
		}
	case "node-peer-add":
		if pm, ok := m["peer"].(map[string]any); ok {
			nl.remotePeerAdd(pm)
		}
	case "node-peer-remove":
		id, _ := m["id"].(string)
		if id != "" {
			nl.hub.remotePeerRemove(nl.id + ":" + id)
		}
	case "node-offer":
		nl.handleOffer(m)
	case "node-answer":
		nl.handleAnswer(m)
	case "node-candidate":
		nl.handleCandidate(m)
	case "node-chat":
		if msg, ok := m["msg"].(map[string]any); ok {
			nl.handleRemoteChat(msg)
		}
	case "node-chat-history":
		// 对端节点同步的聊天历史（握手补齐）：按 from+ts 去重合并
		if arr, ok := m["messages"].([]any); ok {
			msgs := make([]map[string]any, 0, len(arr))
			for _, v := range arr {
				if mm, ok := v.(map[string]any); ok {
					msgs = append(msgs, mm)
				}
			}
			nl.hub.mergeRemoteChatHistory(msgs, nl.id)
		}
	case "node-stream-remove":
		ownerID, _ := m["ownerId"].(string)
		kind, _ := m["kind"].(string)
		if ownerID != "" {
			po := nl.id + ":" + ownerID
			switch kind {
			case "all":
				nl.hub.sfu.removeAudioStreamAll(po)
			case "cam":
				nl.hub.sfu.removeStreamVideo(po+"-cam", nil)
			case "video":
				nl.hub.sfu.removeStreamVideo(po, nil)
			default: // 具体音频通道（mic/system/audio）
				nl.hub.sfu.removeStreamAudio(po, kind)
			}
		}
	case "node-share-config":
		ownerID, _ := m["ownerId"].(string)
		cfg, _ := m["config"].(map[string]any)
		if ownerID != "" && cfg != nil {
			h := nl.hub
			po := nl.id + ":" + ownerID
			h.mu.Lock()
			h.streamCfgs[po] = cfg
			h.mu.Unlock()
			h.broadcast(map[string]any{"type": "share-config", "ownerId": po,
				"frameRate": cfg["frameRate"], "bitrate": cfg["bitrate"], "width": cfg["width"], "height": cfg["height"]})
		}
	case "node-rtts":
		// 母节点同步其子用户延迟（卡片"与主机的延迟"）
		rtts, _ := m["rtts"].(map[string]any)
		if len(rtts) == 0 {
			break
		}
		h := nl.hub
		merged := make(map[string]any, len(rtts))
		h.mu.Lock()
		for uid, r := range rtts {
			pid := nl.id + ":" + uid
			if p := h.peers[pid]; p != nil {
				ms, _ := r.(float64)
				p.mu.Lock()
				p.rttMs = int64(ms)
				p.mu.Unlock()
				merged[pid] = int64(ms)
			}
		}
		h.mu.Unlock()
		if len(merged) > 0 {
			h.broadcast(map[string]any{"type": "peer-rtts", "rtts": merged})
		}
	case "node-announce":
		nl.handleAnnounce(m)
	case "node-subscribe":
		// 对端本地有观看者：开始转发本节点全部本地流（按需转发）
		nl.pmu.Lock()
		nl.subscribed = true
		nl.pmu.Unlock()
		nl.subscribeAllLocal()
	case "node-unsubscribe":
		// 对端本地无观看者：停止转发全部本地流 + 清理本节点代理流
		nl.pmu.Lock()
		nl.subscribed = false
		nl.pmu.Unlock()
		nl.removeAllSent()
		nl.hub.sfu.removeProxyStreams(nl.id)
	case "node-topo":
		// 对端节点同步其邻居列表（mesh 全局拓扑）
		nodeID, _ := m["node"].(string)
		if nodeID == "" {
			break
		}
		h := nl.hub
		var nbs []string
		if arr, ok := m["neighbors"].([]any); ok {
			for _, nb := range arr {
				if s, ok := nb.(string); ok {
					nbs = append(nbs, s)
				}
			}
		}
		h.mu.Lock()
		h.topo[nodeID] = nbs
		h.mu.Unlock()
		h.broadcastTopo()
	case "node-need-negotiate":
		// 对端（非 initiator）新增了轨但无 offer 权：由本端（initiator）发起重协商，避免双方并发 offer（glare）
		if nl.initiator {
			nl.requestNegotiation()
		}
	case "node-file-get":
		nl.handleRemoteFileGet(m)
	case "node-file-chunk":
		nl.handleRemoteFileChunk(m)
	case "node-ping":
		t, _ := m["t"]
		nl.sendMsg(map[string]any{"type": "node-pong", "t": t})
	case "node-pong":
		if t, ok := m["t"].(float64); ok {
			nl.pmu.Lock()
			nl.rttMs = time.Now().UnixMilli() - int64(t)
			nl.pmu.Unlock()
		}
	case "node-link-rtts":
		// 邻居同步的节点间延迟矩阵（近似计算"用户到其他主机延迟"）
		if rtts, ok := m["rtts"].(map[string]any); ok {
			h := nl.hub
			row := make(map[string]int64, len(rtts))
			for nid, r := range rtts {
				if ms, ok := r.(float64); ok {
					row[nid] = int64(ms)
				}
			}
			h.mu.Lock()
			h.linkRtt[nl.id] = row
			h.mu.Unlock()
		}
	}
}

func (nl *nodeLink) handleHello(m map[string]any) {
	h := nl.hub
	id, _ := m["nodeId"].(string)
	if id == "" || id == h.nodeId {
		nl.retry = false // 自连/无效身份：不自动重连
		nl.close()
		return
	}
	nl.pmu.Lock()
	if nl.handshaked {
		// 重复 hello（握手已完成）：仅更新对端再互联凭证，不再回复（防回环风暴）
		if tok, _ := m["forwardToken"].(string); tok != "" {
			nl.peerToken = tok
		}
		nl.pmu.Unlock()
		return
	}
	nl.handshaked = true
	nl.pmu.Unlock()
	h.mu.Lock()
	if old, ok := h.nodes[id]; ok && old != nl {
		old.retry = false // 被新连接替换：旧连接断开不触发自动重连
		go old.close()    // 与同一节点重复连接：保留新连接
	}
	nl.id = id
	if tok, _ := m["forwardToken"].(string); tok != "" {
		nl.peerToken = tok
	}
	h.nodes[id] = nl
	delete(h.linkPending, id)
	peers := make([]map[string]any, 0, len(h.peers))
	for _, p := range h.peers {
		if p.conn != nil { // 只同步本地浏览器用户（虚拟 peer 不回传）
			peers = append(peers, h.peerInfo(p))
		}
	}
	h.mu.Unlock()
	nl.forwardTok = h.issueForwardToken(id) // 锁外签发再互联凭证（h.mu 内调用会重入死锁）
	log.Printf("node: 与节点 %s(%s) 互联建立", id, nl.remote)
	nl.sendMsg(map[string]any{"type": "node-hello", "nodeId": h.nodeId, "forwardToken": nl.forwardTok})
	nl.sendMsg(map[string]any{"type": "node-peers", "peers": peers})
	if nl.initiator {
		nl.pmu.Lock()
		nl.initNodePCLocked()
		nl.negotiateLocked()
		nl.pmu.Unlock()
	}
	h.syncTopology()   // 更新全局拓扑：向所有邻居广播我的邻居列表 + 本地用户刷新
	nl.sendNeighbors() // mesh：告知对方我的其他邻居
	// 握手时同步本节点聊天历史（对方去重合并，新加入节点补齐 100 条）
	h.mu.Lock()
	hist := make([]map[string]any, len(h.chatHist))
	copy(hist, h.chatHist)
	h.mu.Unlock()
	if len(hist) > 0 {
		nl.sendMsg(map[string]any{"type": "node-chat-history", "messages": hist})
	}
}

// syncTopology 更新本节点拓扑并同步：向所有邻居广播我的邻居列表（node-topo），本地浏览器用户刷新 node-info
func (h *hub) syncTopology() {
	h.mu.Lock()
	neighbors := make([]string, 0, len(h.nodes))
	for id := range h.nodes {
		neighbors = append(neighbors, id)
	}
	h.topo[h.nodeId] = neighbors
	links := make([]*nodeLink, 0, len(h.nodes))
	for _, nl := range h.nodes {
		links = append(links, nl)
	}
	h.mu.Unlock()
	for _, nl := range links {
		nl.sendMsg(map[string]any{"type": "node-topo", "node": h.nodeId, "neighbors": neighbors})
	}
	h.broadcastTopo()
}

// broadcastTopo 向本地浏览器用户广播完整拓扑（node-info 带 topo、各节点子用户与本节点信息）
func (h *hub) broadcastTopo() {
	h.broadcast(map[string]any{"type": "node-info", "nodeId": h.nodeId, "myHostName": h.myHostName(), "nodes": h.nodeInfoList(), "topo": h.topoSnapshot(), "myUsers": h.myUserList(), "linkRtts": h.linkRttSnapshot()})
}

// linkRttSnapshot 节点间延迟矩阵拷贝（锁外安全）
func (h *hub) linkRttSnapshot() map[string]map[string]int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := make(map[string]map[string]int64, len(h.linkRtt))
	for k, v := range h.linkRtt {
		row := make(map[string]int64, len(v))
		for nk, nv := range v {
			row[nk] = nv
		}
		snap[k] = row
	}
	return snap
}

// myHostName 本节点服务运行者昵称（host 浏览器用户；无则空串，前端默认"主机"）
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

// topoSnapshot 返回拓扑的拷贝（锁外安全）
func (h *hub) topoSnapshot() map[string][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := make(map[string][]string, len(h.topo))
	for k, v := range h.topo {
		snap[k] = append([]string(nil), v...)
	}
	return snap
}

// issueForwardToken 为邻居节点 id 签发再互联凭证（其他邻居可凭此连接）
func (h *hub) issueForwardToken(forNode string) string {
	tok := randomToken(16)
	h.mu.Lock()
	h.forwardTokens[tok] = forNode
	h.mu.Unlock()
	return tok
}

// sendNeighbors 把自己的其他邻居（id/地址/凭证）告知对端，触发 mesh 自动扩展
func (nl *nodeLink) sendNeighbors() {
	h := nl.hub
	h.mu.Lock()
	nbs := make([]map[string]any, 0, len(h.nodes))
	for id, other := range h.nodes {
		if other == nl || other.peerToken == "" {
			continue
		}
		nbs = append(nbs, map[string]any{"id": id, "addr": other.remote, "token": other.peerToken})
	}
	h.mu.Unlock()
	if len(nbs) > 0 {
		nl.sendMsg(map[string]any{"type": "node-announce", "neighbors": nbs})
	}
}

func (nl *nodeLink) handleAnnounce(m map[string]any) {
	arr, _ := m["neighbors"].([]any)
	h := nl.hub
	for _, nb := range arr {
		nm, ok := nb.(map[string]any)
		if !ok {
			continue
		}
		nid, _ := nm["id"].(string)
		addr, _ := nm["addr"].(string)
		tok, _ := nm["token"].(string)
		if nid == "" || nid == h.nodeId || addr == "" || tok == "" {
			continue
		}
		h.mu.Lock()
		_, connected := h.nodes[nid]
		if connected || h.linkPending[nid] {
			h.mu.Unlock()
			continue
		}
		h.linkPending[nid] = true
		h.mu.Unlock()
		log.Printf("node: 自动互联 %s(%s)", nid, addr)
		go h.dialNode(addr, tok, nl.id)
	}
}

// ---------- 虚拟 peer（远端节点用户） ----------

func (nl *nodeLink) remotePeerAdd(info map[string]any) {
	id, _ := info["id"].(string)
	if id == "" {
		return
	}
	h := nl.hub
	rid := nl.id + ":" + id // 远端用户 id 带节点前缀，防跨节点冲突
	h.mu.Lock()
	p := h.peers[rid]
	if p == nil {
		p = &peer{hub: h, id: rid, conn: nil, send: make(chan []byte, 1)}
		h.peers[rid] = p
		nl.peerIDs[rid] = true
	}
	// 更新式同步：加入/改名/重连后字段保持最新
	p.name, _ = info["name"].(string)
	p.color, _ = info["color"].(string)
	p.host, _ = info["host"].(bool)
	p.ip, _ = info["ip"].(string)
	p.enterHost, _ = info["enterHost"].(string)
	if hid, _ := info["hostId"].(string); hid != "" {
		p.hostId = nl.id + ":" + hid
	}
	h.mu.Unlock()
	h.broadcast(map[string]any{"type": "peer-joined", "id": rid, "name": p.name, "color": p.color, "host": p.host, "ip": p.ip, "enterHost": p.enterHost, "hostId": p.hostId})
	h.broadcastTopo() // 拓扑子用户变化
	log.Printf("node: 远端用户同步 %s(%s)", rid, p.name)
}

func (h *hub) remotePeerRemove(id string) {
	h.mu.Lock()
	if _, ok := h.peers[id]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.peers, id)
	h.mu.Unlock()
	h.broadcast(map[string]any{"type": "peer-left", "id": id})
	h.broadcastTopo() // 拓扑子用户变化
	log.Printf("node: 远端用户离开 %s", id)
}

// ---------- 节点间 WebRTC（双向全量互转三层视频 + 全部音频） ----------

func (nl *nodeLink) initNodePCLocked() {
	if nl.pc != nil {
		return
	}
	h := nl.hub
	pc, err := newPeerConnection(h.sfu.iceServers)
	if err != nil {
		log.Printf("node: 创建节点间 PC 失败 %s: %v", nl.remote, err)
		return
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		nl.sendMsg(map[string]any{"type": "node-candidate", "candidate": c.ToJSON()})
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		nl.onRemoteTrack(remote, recv)
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		if st == webrtc.ICEConnectionStateClosed {
			pc.Close()
		}
	})
	nl.pc = pc
	// 按需转发：仅对端已订阅本节点流时全量 AddTrack（对端稍后 node-subscribe 到达时补转）
	if nl.subscribed {
		h.sfu.mu.Lock()
		sts := make([]*stream, 0, len(h.sfu.streams))
		for _, st := range h.sfu.streams {
			sts = append(sts, st)
		}
		h.sfu.mu.Unlock()
		for _, st := range sts {
			nl.addLocalStreamLocked(st)
		}
	}
	logV("node: 节点间 WebRTC 通道已创建（%s）", nl.remote)
}

// addLocalStream 通知节点间通道转发一个本地流（新层/新音频轨增量）
func (nl *nodeLink) addLocalStream(st *stream) {
	nl.pmu.Lock()
	defer nl.pmu.Unlock()
	nl.addLocalStreamLocked(st)
}

// subscribeAllLocal 对端订阅到达：转发本节点当前全部本地流（锁外调用）
func (nl *nodeLink) subscribeAllLocal() {
	nl.pmu.Lock()
	defer nl.pmu.Unlock()
	if nl.pc == nil {
		return
	}
	h := nl.hub
	h.sfu.mu.Lock()
	sts := make([]*stream, 0, len(h.sfu.streams))
	for _, st := range h.sfu.streams {
		sts = append(sts, st)
	}
	h.sfu.mu.Unlock()
	for _, st := range sts {
		nl.addLocalStreamLocked(st)
	}
}

// removeAllSent 对端取消订阅：移除全部已发送轨（锁外调用）
func (nl *nodeLink) removeAllSent() {
	nl.pmu.Lock()
	defer nl.pmu.Unlock()
	if nl.pc == nil {
		return
	}
	changed := false
	for key, sender := range nl.sent {
		nl.pc.RemoveTrack(sender) //nolint:errcheck
		sender.Stop()
		delete(nl.sent, key)
		changed = true
	}
	if changed {
		nl.requestNegotiationLocked()
	}
}

// syncSubscribeToNeighbors 本地观看者数量变化时通知所有邻居（按需转发：有人看→订阅，无人→取消）
func (h *hub) syncSubscribeToNeighbors() {
	h.sfu.mu.Lock()
	viewerCount := len(h.sfu.viewers)
	h.sfu.mu.Unlock()
	msgType := "node-unsubscribe"
	if viewerCount > 0 {
		msgType = "node-subscribe"
	}
	h.mu.Lock()
	links := make([]*nodeLink, 0, len(h.nodes))
	for _, nl := range h.nodes {
		links = append(links, nl)
	}
	h.mu.Unlock()
	for _, nl := range links {
		nl.sendMsg(map[string]any{"type": msgType})
	}
}

func (nl *nodeLink) addLocalStreamLocked(st *stream) {
	if nl.pc == nil {
		return
	}
	changed := false
	for rid, l := range st.Layers {
		key := st.ID + "|" + rid
		if _, ok := nl.sent[key]; ok {
			continue
		}
		sender, err := nl.pc.AddTrack(l)
		if err != nil {
			log.Printf("node: AddTrack 失败 %s: %v", key, err)
			continue
		}
		nl.sent[key] = sender
		changed = true
	}
	for kind, al := range st.AudioTracks {
		key := st.ID + "|" + kind
		if _, ok := nl.sent[key]; ok {
			continue
		}
		sender, err := nl.pc.AddTrack(al)
		if err != nil {
			log.Printf("node: 音频 AddTrack 失败 %s: %v", key, err)
			continue
		}
		nl.sent[key] = sender
		changed = true
	}
	if changed {
		nl.requestNegotiationLocked()
	}
}

// removeLocalStream 移除已发送给对端的流（按流前缀，如整流/整层移除）
func (nl *nodeLink) removeLocalStream(streamID string) {
	nl.pmu.Lock()
	defer nl.pmu.Unlock()
	if nl.pc == nil {
		return
	}
	changed := false
	for key, sender := range nl.sent {
		if strings.HasPrefix(key, streamID+"|") {
			nl.pc.RemoveTrack(sender) //nolint:errcheck
			sender.Stop()
			delete(nl.sent, key)
			changed = true
		}
	}
	if changed {
		nl.requestNegotiationLocked()
	}
}

// removeLocalStreamKey 精确移除单个轨（如某个音频通道）
func (nl *nodeLink) removeLocalStreamKey(streamID, suffix string) {
	nl.pmu.Lock()
	defer nl.pmu.Unlock()
	if nl.pc == nil {
		return
	}
	key := streamID + "|" + suffix
	sender, ok := nl.sent[key]
	if !ok {
		return
	}
	nl.pc.RemoveTrack(sender) //nolint:errcheck
	sender.Stop()
	delete(nl.sent, key)
	nl.requestNegotiationLocked()
}

// requestNegotiationLocked 协商请求：仅 initiator 有权发 offer（避免双方并发 offer 的 glare）；
// 非 initiator 通知对端需要协商（对端在 stable 时发起）
func (nl *nodeLink) requestNegotiationLocked() {
	if nl.pc == nil {
		return
	}
	if !nl.initiator {
		nl.sendMsg(map[string]any{"type": "node-need-negotiate"})
		return
	}
	nl.negotiateLocked()
}

func (nl *nodeLink) requestNegotiation() {
	nl.pmu.Lock()
	defer nl.pmu.Unlock()
	nl.requestNegotiationLocked()
}

func (nl *nodeLink) negotiateLocked() {
	if nl.pc == nil {
		return
	}
	if nl.pc.SignalingState() != webrtc.SignalingStateStable {
		nl.pending = true // 有在途协商：等对方 answer 完成后补发
		return
	}
	offer, err := nl.pc.CreateOffer(nil)
	if err != nil {
		return
	}
	if err := nl.pc.SetLocalDescription(offer); err != nil {
		return
	}
	nl.sendMsg(map[string]any{"type": "node-offer", "sdp": offer})
}

func (nl *nodeLink) handleOffer(m map[string]any) {
	sd, _ := m["sdp"].(map[string]any)
	sdpStr, _ := sd["sdp"].(string)
	if sdpStr == "" {
		return
	}
	nl.pmu.Lock()
	if nl.pc == nil {
		nl.initNodePCLocked()
	}
	pc := nl.pc
	err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdpStr})
	if err == nil {
		var answer webrtc.SessionDescription
		answer, err = pc.CreateAnswer(nil)
		if err == nil {
			err = pc.SetLocalDescription(answer)
		}
		nl.pmu.Unlock()
		if err == nil {
			nl.sendMsg(map[string]any{"type": "node-answer", "sdp": answer})
		}
		return
	}
	nl.pmu.Unlock()
	log.Printf("node: 处理 offer 失败: %v", err)
}

func (nl *nodeLink) handleAnswer(m map[string]any) {
	sd, _ := m["sdp"].(map[string]any)
	sdpStr, _ := sd["sdp"].(string)
	nl.pmu.Lock()
	if nl.pc == nil {
		nl.pmu.Unlock()
		return
	}
	err := nl.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdpStr})
	pending := nl.pending
	nl.pending = false
	nl.pmu.Unlock()
	if err != nil {
		log.Printf("node: 处理 answer 失败: %v", err)
		return
	}
	if pending {
		go nl.requestNegotiation()
	}
}

func (nl *nodeLink) handleCandidate(m map[string]any) {
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
	nl.pmu.Lock()
	pc := nl.pc
	nl.pmu.Unlock()
	if pc != nil {
		pc.AddICECandidate(c) //nolint:errcheck
	}
}

// onRemoteTrack 收到远端节点转发的流：以代理流注入本地 SFU（复用 onStreamLayer/onStreamAudio）
func (nl *nodeLink) onRemoteTrack(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	msid := remote.StreamID()
	if remote.Kind() == webrtc.RTPCodecTypeAudio {
		// msid 形如 {owner}-a-{kind}（rewriteTrack 的 StreamID）
		idx := strings.LastIndex(msid, "-a-")
		kind := "audio"
		owner := msid
		if idx >= 0 {
			kind = msid[idx+3:]
			owner = msid[:idx]
		}
		nl.hub.onStreamAudio(nl.id+":"+owner, remote, nl.pc, kind)
		return
	}
	rid := remote.RID()
	if rid == "" {
		rid = "m"
	}
	owner := msid
	if i := strings.LastIndex(owner, "-cam"); i > 0 {
		owner = owner[:i]
	}
	nl.hub.onStreamLayer(nl.id+":"+owner, remote, rid, nl.pc, nl.id+":"+msid)
	// PLI 链：代理流观看者请求关键帧 -> 节点间 PC RTCP -> 按 SSRC 定向转发给源浏览器
	nl.pmu.Lock()
	nl.ssrcMap[uint32(remote.SSRC())] = msid
	if !nl.rtcpOnce[recv] {
		nl.rtcpOnce[recv] = true
		go nl.rtcpLoop(recv)
	}
	nl.pmu.Unlock()
}

// rtcpLoop 读取节点间 PC 上代理流的 RTCP，把关键帧请求转发给源节点的对应流
func (nl *nodeLink) rtcpLoop(recv *webrtc.RTPReceiver) {
	for {
		pkts, _, err := recv.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			pli, ok := pkt.(*rtcp.PictureLossIndication)
			if !ok {
				continue
			}
			nl.pmu.Lock()
			orig := nl.ssrcMap[pli.MediaSSRC]
			nl.pmu.Unlock()
			if orig != "" {
				nl.hub.requestLocalKeyFrame(orig)
			} else {
				nl.hub.requestAllLocalKeyFrames()
			}
		}
	}
}

// requestLocalKeyFrame 向本地源流的各层请求关键帧（跨节点 PLI 转发）
func (h *hub) requestLocalKeyFrame(streamID string) {
	s := h.sfu
	s.mu.Lock()
	st := s.streams[streamID]
	var locs []*rewriteTrack
	if st != nil {
		locs = make([]*rewriteTrack, 0, len(st.Layers))
		for _, l := range st.Layers {
			locs = append(locs, l)
		}
	}
	s.mu.Unlock()
	for _, l := range locs {
		l.requestKeyFrame()
	}
}

// requestAllLocalKeyFrames 兜底：SSRC 未匹配时向全部本地流请求关键帧
func (h *hub) requestAllLocalKeyFrames() {
	s := h.sfu
	s.mu.Lock()
	var locs []*rewriteTrack
	for _, st := range s.streams {
		for _, l := range st.Layers {
			locs = append(locs, l)
		}
	}
	s.mu.Unlock()
	for _, l := range locs {
		l.requestKeyFrame()
	}
}

// ---------- 流变化通知（main.go 钩子调用） ----------

// nodeStreamAdded 本地新流（层/音频轨）到达：通知所有邻居转发（代理流除外，防回环）
func (h *hub) nodeStreamAdded(st *stream) {
	h.mu.Lock()
	links := make([]*nodeLink, 0, len(h.nodes))
	for _, nl := range h.nodes {
		links = append(links, nl)
	}
	h.mu.Unlock()
	for _, nl := range links {
		if nl.pc != nil && nl.pc == st.SourcePC {
			continue // 代理流不回传（源已是远端）
		}
		nl.pmu.Lock()
		sub := nl.subscribed
		nl.pmu.Unlock()
		if !sub {
			continue // 按需转发：对端无观看者订阅时跳过
		}
		nl.addLocalStream(st)
	}
}

// isProxyStream 判断流是否为远端节点的代理流（其源 PC 是某邻居的节点间 PC）
func (h *hub) isProxyStream(st *stream) bool {
	if st == nil || st.SourcePC == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, nl := range h.nodes {
		if nl.pc != nil && nl.pc == st.SourcePC {
			return true
		}
	}
	return false
}

// nodeStreamRemoved 本地流移除：通知邻居移除代理 + 本端通道 RemoveTrack
func (h *hub) nodeStreamRemoved(ownerID, kind string) {
	h.mu.Lock()
	links := make([]*nodeLink, 0, len(h.nodes))
	for _, nl := range h.nodes {
		links = append(links, nl)
	}
	h.mu.Unlock()
	for _, nl := range links {
		switch kind {
		case "cam":
			nl.removeLocalStream(ownerID + "-cam")
		case "video":
			nl.removeLocalStream(ownerID)
		case "all":
			nl.removeLocalStream(ownerID + "-a")
		default: // 具体音频通道（mic/system/audio）：精确移除该轨
			nl.removeLocalStreamKey(ownerID+"-a", kind)
		}
		nl.sendMsg(map[string]any{"type": "node-stream-remove", "ownerId": ownerID, "kind": kind})
	}
}

// removeProxyStreams 节点断开：删除该节点全部代理流并通知观看者
func (s *SFU) removeProxyStreams(nodeID string) {
	prefix := nodeID + ":"
	s.mu.Lock()
	var vids, auds []string
	for id := range s.streams {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		if strings.HasSuffix(id, "-a") {
			auds = append(auds, id)
		} else {
			vids = append(vids, id)
		}
	}
	if len(vids) == 0 && len(auds) == 0 {
		s.mu.Unlock()
		return
	}
	toNegotiate := make(map[*viewerConn]bool)
	for _, id := range vids {
		delete(s.streams, id)
		for _, vc := range s.viewers {
			if sender, ok := vc.senders[id]; ok {
				vc.pc.RemoveTrack(sender) //nolint:errcheck
				sender.Stop()
				delete(vc.senders, id)
				delete(vc.tiers, id)
				toNegotiate[vc] = true
			}
		}
	}
	for _, id := range auds {
		delete(s.streams, id)
		ap := id + "-"
		for _, vc := range s.viewers {
			for key, aSender := range vc.audioSender {
				if strings.HasPrefix(key, ap) {
					vc.pc.RemoveTrack(aSender) //nolint:errcheck
					aSender.Stop()
					delete(vc.audioSender, key)
					toNegotiate[vc] = true
				}
			}
		}
	}
	s.mu.Unlock()
	for vc := range toNegotiate {
		vc.negotiate()
	}
	for _, id := range vids {
		s.hub.broadcast(map[string]any{"type": "stream-removed", "id": id, "ownerId": id})
	}
	for _, id := range auds {
		s.hub.broadcast(map[string]any{"type": "stream-removed", "id": id, "ownerId": strings.TrimSuffix(id, "-a")})
	}
	log.Printf("node: 清理节点 %s 的代理流 %d 个", nodeID, len(vids)+len(auds))
}

// ---------- 延迟探测（卡片"与主机的延迟"） ----------

// rttProbe 周期测量各用户到其主机的延迟：服务器主动 ping 本地浏览器；
// 同时测量节点间直连延迟并同步矩阵（拓扑连线标注 + 用户到其他主机近似）
func (h *hub) rttProbe() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixMilli()
		h.mu.Lock()
		local := make([]*peer, 0, len(h.peers))
		links := make([]*nodeLink, 0, len(h.nodes))
		for _, p := range h.peers {
			if p.conn != nil {
				local = append(local, p)
			}
		}
		for _, nl := range h.nodes {
			links = append(links, nl)
		}
		h.mu.Unlock()
		for _, p := range local {
			sendMsg(p, map[string]any{"type": "ping", "t": now})
		}
		// 死连接探测：连续 20s 无 pong 的浏览器连接主动断开（触发其本地立即重连，
		// 避免半死连接挂到系统 TCP 超时——服务进程消失/手机网络半断场景）
		nowMs := time.Now().UnixMilli()
		for _, p := range local {
			p.mu.Lock()
			dead := nowMs-p.lastPong > 20000 && p.lastPong > 0
			age := nowMs - p.lastPong
			p.mu.Unlock()
			if dead {
				log.Printf("peer: %s(%s) 心跳超时（%dms 无响应），主动断开", p.id, p.name, age)
				p.conn.Close() // 触发 readLoop 退出 → unregister 清理
			}
		}
		// 节点间延迟探测 + 更新本节点行
		linkRtts := make(map[string]int64)
		for _, nl := range links {
			nl.sendMsg(map[string]any{"type": "node-ping", "t": now})
			nl.pmu.Lock()
			r := nl.rttMs
			nl.pmu.Unlock()
			if r > 0 {
				linkRtts[nl.id] = r
			}
		}
		if len(linkRtts) > 0 {
			h.mu.Lock()
			h.linkRtt[h.nodeId] = linkRtts
			h.mu.Unlock()
		}
		// 同步矩阵给邻居（mesh 汇聚完整矩阵）
		h.mu.Lock()
		snap := make(map[string]map[string]int64, len(h.linkRtt))
		for k, v := range h.linkRtt {
			row := make(map[string]int64, len(v))
			for nk, nv := range v {
				row[nk] = nv
			}
			snap[k] = row
		}
		h.mu.Unlock()
		for _, nl := range links {
			nl.sendMsg(map[string]any{"type": "node-link-rtts", "rtts": snap})
		}
		// 用户延迟广播
		h.mu.Lock()
		rtts := make(map[string]any, len(h.peers))
		localRtts := make(map[string]any)
		for id, p := range h.peers {
			p.mu.Lock()
			r := p.rttMs
			p.mu.Unlock()
			if r > 0 {
				rtts[id] = r
				if p.conn != nil {
					localRtts[id] = r
				}
			}
		}
		h.mu.Unlock()
		if len(rtts) > 0 {
			h.broadcast(map[string]any{"type": "peer-rtts", "rtts": rtts})
		}
		if len(localRtts) > 0 {
			for _, nl := range links {
				nl.sendMsg(map[string]any{"type": "node-rtts", "rtts": localRtts})
			}
		}
	}
}

// ---------- 节点信息 / 断开 ----------

func (h *hub) nodeInfoList() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	nodes := make([]map[string]any, 0, len(h.nodes))
	for id, nl := range h.nodes {
		var users []map[string]any
		prefix := id + ":"
		for _, p := range h.peers {
			if p.conn == nil && strings.HasPrefix(p.id, prefix) {
				users = append(users, map[string]any{"id": p.id, "name": p.name})
			}
		}
		nl.pmu.Lock()
		r := nl.rttMs
		nl.pmu.Unlock()
		nodes = append(nodes, map[string]any{"id": id, "remote": nl.remote, "users": users, "rtt": r})
	}
	return nodes
}

// myUserList 本节点浏览器用户（含自己，拓扑图展示子用户分布）
func (h *hub) myUserList() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	users := make([]map[string]any, 0, len(h.peers))
	for _, p := range h.peers {
		if p.conn != nil {
			users = append(users, map[string]any{"id": p.id, "name": p.name})
		}
	}
	return users
}

func (nl *nodeLink) close() {
	nl.pmu.Lock()
	if nl.closed {
		nl.pmu.Unlock()
		return
	}
	nl.closed = true
	pc := nl.pc
	nl.pc = nil
	nl.pmu.Unlock()
	if pc != nil {
		pc.Close()
	}
	h := nl.hub
	var rm []string
	h.mu.Lock()
	if nl.id != "" {
		if h.nodes[nl.id] == nl {
			delete(h.nodes, nl.id)
		}
		// 清理拓扑中该节点的条目（含其邻居列表里指向它的引用）
		delete(h.topo, nl.id)
		for id, nbs := range h.topo {
			kept := nbs[:0]
			for _, nb := range nbs {
				if nb != nl.id {
					kept = append(kept, nb)
				}
			}
			h.topo[id] = kept
		}
		for id := range nl.peerIDs {
			rm = append(rm, id)
		}
		for fid, src := range h.fileRoute {
			if src == nl.id {
				delete(h.fileRoute, fid)
			}
		}
	}
	h.mu.Unlock()
	for _, id := range rm {
		h.remotePeerRemove(id)
	}
	if nl.id != "" {
		h.sfu.removeProxyStreams(nl.id)
		log.Printf("node: 与节点 %s(%s) 断开", nl.id, nl.remote)
		h.syncTopology() // 拓扑变化：通知邻居 + 本地用户刷新
	}
	nl.conn.Close()
	close(nl.send)
	// 发起方自动重连（指数退避 5s→60s）：源节点重启/网络恢复后自动重新互联
	if nl.retry && nl.initiator && nl.remote != "" {
		go h.reconnectNode(nl.remote)
	}
}

// reconnectNode 节点断线自动重连：指数退避（5s 起，×2 封顶 60s）持续尝试，
// 走完整 join 流程（POST join-token → dialNode，无状态依赖）；已重新互联则退出
func (h *hub) reconnectNode(remote string) {
	delay := 5 * time.Second
	for {
		time.Sleep(delay)
		h.mu.Lock()
		dup := false
		for _, nl := range h.nodes {
			if normHost(nl.remote) == normHost(remote) {
				dup = true
				break
			}
		}
		h.mu.Unlock()
		if dup {
			return // 已重新互联
		}
		token := ""
		client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		if resp, err := client.Post(strings.TrimSuffix(remote, "/")+"/api/join-token", "application/json", nil); err == nil {
			var jr struct {
				Token string `json:"token"`
			}
			if json.NewDecoder(resp.Body).Decode(&jr) == nil {
				token = jr.Token
			}
			resp.Body.Close()
		}
		if token != "" {
			go h.dialNode(remote, token, "") // 异步：Dial 成功进入 readLoop；失败仅日志
			log.Printf("node: 自动重连 %s", remote)
		}
		delay *= 2
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
	}
}

// ---------- 跨节点聊天与文件 ----------

func (nl *nodeLink) handleRemoteChat(msg map[string]any) {
	h := nl.hub
	if fid, _ := msg["fileId"].(string); fid != "" {
		h.mu.Lock()
		h.fileRoute[fid] = nl.id
		h.mu.Unlock()
	}
	h.appendChat(msg)
}

// mergeRemoteChatHistory 合并对端节点的聊天历史（from+ts 去重；新消息广播本地用户并设置文件路由）
func (h *hub) mergeRemoteChatHistory(msgs []map[string]any, srcNode string) {
	if len(msgs) == 0 {
		return
	}
	newMsgs := make([]map[string]any, 0, len(msgs))
	h.mu.Lock()
	seen := make(map[string]bool, len(h.chatHist))
	for _, m := range h.chatHist {
		from, _ := m["from"].(string)
		ts, _ := m["ts"].(float64)
		seen[from+"|"+fmt.Sprintf("%.0f", ts)] = true
	}
	for _, m := range msgs {
		from, _ := m["from"].(string)
		ts, _ := m["ts"].(float64)
		key := from + "|" + fmt.Sprintf("%.0f", ts)
		if seen[key] {
			continue
		}
		seen[key] = true
		h.chatHist = append(h.chatHist, m)
		newMsgs = append(newMsgs, m)
		if fid, _ := m["fileId"].(string); fid != "" {
			h.fileRoute[fid] = srcNode
		}
	}
	// 历史超上限：截断最旧（保持最近 100 条）
	if len(h.chatHist) > 100 {
		h.chatHist = h.chatHist[len(h.chatHist)-100:]
	}
	h.mu.Unlock()
	for _, m := range newMsgs {
		h.broadcast(m) // 新历史对本地用户可见（前端 appendChatMsg 渲染）
	}
}

// fetchRemoteFile 经节点 WS 向源节点分片拉取聊天文件（本地缓存未命中时）
func (h *hub) fetchRemoteFile(fileID, nodeID string) *chatFile {
	ch := make(chan *chatFile, 1)
	h.mu.Lock()
	if h.fileWait[fileID] == nil {
		h.fileBuf[fileID] = nil
	}
	h.fileWait[fileID] = append(h.fileWait[fileID], ch)
	nl := h.nodes[nodeID]
	h.mu.Unlock()
	if nl == nil {
		return nil
	}
	nl.sendMsg(map[string]any{"type": "node-file-get", "fileId": fileID})
	select {
	case cf := <-ch:
		return cf
	case <-time.After(30 * time.Second):
		h.mu.Lock()
		delete(h.fileBuf, fileID)
		h.mu.Unlock()
		return nil
	}
}

// node-file-get 源节点处理：从缓存分片回传
func (nl *nodeLink) handleRemoteFileGet(m map[string]any) {
	fileID, _ := m["fileId"].(string)
	h := nl.hub
	h.mu.Lock()
	cf := h.chatFiles[fileID]
	h.mu.Unlock()
	if cf == nil {
		nl.sendMsg(map[string]any{"type": "node-file-chunk", "fileId": fileID, "total": 0})
		return
	}
	data := cf.data
	for i := 0; i < len(data); i += nodeFileChunkSize {
		end := i + nodeFileChunkSize
		if end > len(data) {
			end = len(data)
		}
		msg := map[string]any{
			"type": "node-file-chunk", "fileId": fileID,
			"idx": i, "total": len(data),
			"data": base64.StdEncoding.EncodeToString(data[i:end]),
		}
		if i == 0 {
			msg["mime"] = cf.mime
			msg["name"] = cf.name
		}
		nl.sendMsg(msg)
	}
}

// node-file-chunk 请求端组装：分片按序到达（WS 有序），收齐后缓存并唤醒下载等待者
func (nl *nodeLink) handleRemoteFileChunk(m map[string]any) {
	fileID, _ := m["fileId"].(string)
	total, _ := m["total"].(float64)
	h := nl.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if int(total) == 0 {
		// 源节点无此文件：唤醒等待者（nil）
		if waiters, ok := h.fileWait[fileID]; ok {
			delete(h.fileWait, fileID)
			delete(h.fileBuf, fileID)
			for _, ch := range waiters {
				close(ch)
			}
		}
		return
	}
	b64, _ := m["data"].(string)
	chunk, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return
	}
	buf := h.fileBuf[fileID]
	buf = append(buf, chunk...)
	if len(buf) >= int(total) {
		mime, _ := m["mime"].(string)
		name, _ := m["name"].(string)
		cf := &chatFile{data: buf[:int(total)], mime: mime, name: name, at: time.Now(), big: int(total) > chatBigFileSize}
		h.chatFiles[fileID] = cf
		h.chatTotalB += len(cf.data)
		if waiters, ok := h.fileWait[fileID]; ok {
			delete(h.fileWait, fileID)
			delete(h.fileBuf, fileID)
			for _, ch := range waiters {
				select {
				case ch <- cf:
				default:
				}
			}
		}
		log.Printf("chat-file: 跨节点拉取 %s (%d 字节)", fileID, int(total))
		return
	}
	h.fileBuf[fileID] = buf
}
