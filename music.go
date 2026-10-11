package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------- 音乐播放器（共享听歌）：平台解析抽象，分类预留扩展 ----------

// MusicItem 一首歌（解析结果，加入播放列表）
type MusicItem struct {
	ID            string `json:"id"`            // 平台内曲目 id（stream/lyric 用）
	Title         string `json:"title"`         // 标题
	Artist        string `json:"artist"`        // 歌手
	Cover         string `json:"cover"`         // 封面 URL（可选）
	URL           string `json:"url"`           // 原始链接（直链/网易云页）
	Source        string `json:"source"`        // 平台分类：netease / direct（预留 qq/kugou 等）
	Dur           int    `json:"dur"`           // 时长（秒，0=未知）
	PlaylistID    string `json:"playlistId"`    // 所属歌单 id（分组 key；空=单曲/直链）
	PlaylistTitle string `json:"playlistTitle"` // 所属歌单名（显示用）
	PlaylistCover string `json:"playlistCover"` // 所属歌单封面（显示用）
	Album         string `json:"album"`         // 专辑名
	TransName     string `json:"transName"`     // 歌曲译名（transNames[0]，可选）
	Quality       int    `json:"quality"`       // 音质 kbps（320/128，0=未知）
	Pop           int    `json:"pop"`           // 热度 0-100（点赞数替代显示）
	TrackCount    int    `json:"trackCount"`    // 歌单歌曲数（搜索结果展示）
	Fee           int    `json:"fee"`           // 收费标记：0=免费 1=VIP 8=付费单曲（搜索结果标记）
}

// musicParser 平台解析接口：新增平台实现此接口并注册即可
type musicParser interface {
	Match(url string) bool
	Parse(url string) ([]MusicItem, error) // 歌单展开为多首
}

var musicParsers = []musicParser{&neteaseParser{}, &directParser{}}

// extractPlaylistID 从网易云 URL 提取歌单 id（playlist?id=xxx / playlist/xxx）
func extractPlaylistID(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "playlist?id="); i >= 0 {
		s := raw[i+len("playlist?id="):]
		if j := strings.IndexAny(s, "&# "); j >= 0 {
			s = s[:j]
		}
		return strings.TrimSpace(s)
	}
	if i := strings.Index(raw, "playlist/"); i >= 0 {
		s := raw[i+len("playlist/"):]
		if j := strings.IndexAny(s, "&# /?"); j >= 0 {
			s = s[:j]
		}
		return strings.TrimSpace(s)
	}
	return ""
}

// FetchPlaylistByID 按歌单 id 拉取最新歌曲（供刷新用）
func FetchPlaylistByID(pid string) ([]MusicItem, error) {
	return neteasePlaylist(pid)
}

func parseMusicURL(u string) ([]MusicItem, error) {
	for _, p := range musicParsers {
		if p.Match(u) {
			return p.Parse(u)
		}
	}
	return nil, fmt.Errorf("不支持的链接格式")
}

// ---------- 网易云（无登录态，Referer 防盗链） ----------

type neteaseParser struct{}

var (
	neteaseSongRe     = regexp.MustCompile(`music\.163\.com/(?:#/|m/)?song\?id=(\d+)`)
	neteaseSongAltRe  = regexp.MustCompile(`music\.163\.com/(?:#/|m/)?song/(\d+)`)
	neteasePlaylistRe = regexp.MustCompile(`music\.163\.com/(?:#/|m/)?playlist\?id=(\d+)`)
)

func (p *neteaseParser) Match(u string) bool { return strings.Contains(u, "music.163.com") }

func (p *neteaseParser) Parse(u string) ([]MusicItem, error) {
	if m := neteasePlaylistRe.FindStringSubmatch(u); m != nil {
		return neteasePlaylist(m[1])
	}
	if m := neteaseSongRe.FindStringSubmatch(u); m != nil {
		return neteaseSongs([]string{m[1]})
	}
	if m := neteaseSongAltRe.FindStringSubmatch(u); m != nil {
		return neteaseSongs([]string{m[1]})
	}
	return nil, fmt.Errorf("无法识别的网易云链接")
}

// neteaseReq 网易云 API 请求（POST 表单，带 Referer；无登录态可获取普通音质）
func neteaseReq(path string, form map[string]string) (map[string]any, error) {
	body := url.Values{}
	for k, v := range form {
		body.Set(k, v)
	}
	req, err := http.NewRequest("POST", "https://music.163.com"+path, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := neteaseHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

var neteaseHTTP = &http.Client{Timeout: 15 * time.Second}

// neteaseSongs 按 id 列表解析（旧 /api/song/detail 拿详情 + player/url 拿直链）
func neteaseSongs(ids []string) ([]MusicItem, error) {
	var items []MusicItem
	if len(ids) == 0 {
		return items, nil
	}
	// 详情（v3 接口 POST form c=[{"id":..}]；旧 /api/song/detail 已返回 405 废弃）
	var cparts []string
	for _, id := range ids {
		cparts = append(cparts, `{"id":`+id+`}`)
	}
	det, err := neteaseReq("/api/v3/song/detail", map[string]string{"c": "[" + strings.Join(cparts, ",") + "]"})
	if err == nil {
		if songs, ok := det["songs"].([]any); ok {
			for _, s := range songs {
				sm, _ := s.(map[string]any)
				item := MusicItem{
					ID: fmt.Sprintf("%.0f", num(sm["id"])), Source: "netease",
					Title:  str(sm["name"]),
					Artist: neteaseArtistsShort(sm),   // v3 接口缩写字段 ar
					Dur:    int(num(sm["dt"]) / 1000), // dt 毫秒
					Pop:    int(num(sm["pop"])),
				}
				if alb, ok := sm["al"].(map[string]any); ok {
					item.Album = str(alb["name"])
					item.Cover = str(alb["picUrl"])
				}
				if tns, ok := sm["tns"].([]any); ok && len(tns) > 0 {
					item.TransName = str(tns[0])
				}
				items = append(items, item)
			}
		}
	}
	neteaseFillURLs(items)
	return filterPlayable(items), nil
}

// neteaseSearch 网易云歌曲搜索（关键字 → 歌曲列表，无直链；添加时才解析）
func neteaseSearch(keyword string, limit int) ([]MusicItem, error) {
	if limit <= 0 || limit > 50 {
		limit = 15
	}
	det, err := neteaseReq("/api/search/get", map[string]string{
		"s": keyword, "type": "1", "offset": "0", "limit": strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}
	res, _ := det["result"].(map[string]any)
	songs, _ := res["songs"].([]any)
	var items []MusicItem
	for _, s := range songs {
		sm, _ := s.(map[string]any)
		if int(num(sm["fee"])) > 0 {
			continue // VIP/付费歌曲：无登录态不可播，搜索结果直接过滤
		}
		item := MusicItem{
			ID: fmt.Sprintf("%.0f", num(sm["id"])), Source: "netease",
			Title:  str(sm["name"]),
			Artist: neteaseArtists(sm),
			Dur:    int(num(sm["duration"]) / 1000),
			Fee:    int(num(sm["fee"])),
		}
		if alb, ok := sm["album"].(map[string]any); ok {
			item.Album = str(alb["name"])
			item.Cover = str(alb["picUrl"])
		}
		items = append(items, item)
	}
	neteaseSearchQuality(items) // 批量查音质（best effort，失败忽略）
	neteaseSearchMeta(items)    // 批量补封面/专辑（搜索接口 album 无 picUrl，v3/detail 有）
	return items, nil
}

// neteaseSearchMeta 批量补元数据（封面/专辑/译名：v3/song/detail 的 al/tns 字段）
func neteaseSearchMeta(items []MusicItem) {
	if len(items) == 0 {
		return
	}
	var cparts []string
	for _, it := range items {
		cparts = append(cparts, `{"id":`+it.ID+`}`)
	}
	det, err := neteaseReq("/api/v3/song/detail", map[string]string{"c": "[" + strings.Join(cparts, ",") + "]"})
	if err != nil {
		return
	}
	if songs, ok := det["songs"].([]any); ok {
		for _, s := range songs {
			sm, _ := s.(map[string]any)
			id := fmt.Sprintf("%.0f", num(sm["id"]))
			for i := range items {
				if items[i].ID != id {
					continue
				}
				if alb, ok := sm["al"].(map[string]any); ok {
					items[i].Album = str(alb["name"])
					items[i].Cover = str(alb["picUrl"])
				}
				if tns, ok := sm["tns"].([]any); ok && len(tns) > 0 {
					items[i].TransName = str(tns[0])
				}
				break
			}
		}
	}
}

// neteaseSearchQuality 批量查音质（player/url br；不存直链，避免 URL 时效问题）
func neteaseSearchQuality(items []MusicItem) {
	if len(items) == 0 {
		return
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	urls, err := neteaseReq("/api/song/enhance/player/url", map[string]string{
		"ids": "[" + strings.Join(ids, ",") + "]", "br": "320000",
	})
	if err != nil {
		return
	}
	if data, ok := urls["data"].([]any); ok {
		byID := map[string]int{}
		for _, d := range data {
			dm, _ := d.(map[string]any)
			byID[fmt.Sprintf("%.0f", num(dm["id"]))] = int(num(dm["br"]) / 1000)
		}
		for i := range items {
			if br, ok := byID[items[i].ID]; ok && br > 0 {
				items[i].Quality = br
			}
		}
	}
}

// neteaseSearchPlaylist 网易云歌单搜索（关键字 → 歌单列表；条目用 PlaylistCover/PlaylistTitle 存封面/名称）
func neteaseSearchPlaylist(keyword string, limit int) ([]MusicItem, error) {
	if limit <= 0 || limit > 50 {
		limit = 15
	}
	det, err := neteaseReq("/api/search/get", map[string]string{
		"s": keyword, "type": "1000", "offset": "0", "limit": strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}
	res, _ := det["result"].(map[string]any)
	pls, _ := res["playlists"].([]any)
	var items []MusicItem
	for _, p := range pls {
		pm, _ := p.(map[string]any)
		items = append(items, MusicItem{
			ID: fmt.Sprintf("%.0f", num(pm["id"])), Source: "netease",
			Title:         str(pm["name"]),
			PlaylistID:    fmt.Sprintf("%.0f", num(pm["id"])),
			PlaylistTitle: str(pm["name"]),
			PlaylistCover: str(pm["coverImgUrl"]),
			Dur:           int(num(pm["playCount"])),  // 播放数（时长字段复用）
			TrackCount:    int(num(pm["trackCount"])), // 歌曲数
		})
	}
	return items, nil
}

func neteasePlaylist(pid string) ([]MusicItem, error) {
	det, err := neteaseReq("/api/v6/playlist/detail", map[string]string{"id": pid, "n": "100000"})
	if err != nil {
		return nil, err
	}
	pl, _ := det["playlist"].(map[string]any)
	plTitle := str(pl["name"])        // 歌单名（分组显示）
	plCover := str(pl["coverImgUrl"]) // 歌单封面（卡片显示）
	// 优先 trackIds（完整歌单全部曲目 id，可能几百首）；tracks 字段只是接口预览的前几首
	if tids, ok := pl["trackIds"].([]any); ok && len(tids) > 0 {
		var all []MusicItem
		var batch []string
		for _, ti := range tids {
			tm, _ := ti.(map[string]any)
			batch = append(batch, fmt.Sprintf("%.0f", num(tm["id"])))
			if len(batch) >= 100 { // 分批（详情+直链接口批量上限）
				items, err := neteaseSongs(batch)
				if err != nil {
					return nil, fmt.Errorf("歌单曲目解析失败: %v", err)
				}
				for i := range items {
					items[i].PlaylistID = pid
					items[i].PlaylistTitle = plTitle
					items[i].PlaylistCover = plCover
				}
				all = append(all, items...)
				batch = nil
			}
		}
		if len(batch) > 0 {
			items, err := neteaseSongs(batch)
			if err != nil {
				return nil, fmt.Errorf("歌单曲目解析失败: %v", err)
			}
			for i := range items {
				items[i].PlaylistID = pid
				items[i].PlaylistTitle = plTitle
				items[i].PlaylistCover = plCover
			}
			all = append(all, items...)
		}
		return all, nil
	}
	// 降级：tracks 预览字段（接口异常时兜底）
	tracks, _ := pl["tracks"].([]any)
	var items []MusicItem
	for _, t := range tracks {
		tm, _ := t.(map[string]any)
		items = append(items, MusicItem{
			ID: fmt.Sprintf("%.0f", num(tm["id"])), Source: "netease",
			Title:      str(tm["name"]),
			Artist:     neteaseArtistsShort(tm),   // 歌单接口用缩写字段 ar
			Cover:      neteaseCoverShort(tm),     // al.picUrl
			Dur:        int(num(tm["dt"]) / 1000), // dt 毫秒
			PlaylistID: pid, PlaylistTitle: plTitle, PlaylistCover: plCover,
		})
	}
	neteaseFillURLs(items)
	return filterPlayable(items), nil
}

// neteaseFillURLs 批量补直链（player/url，br=320000；无登录态拿可用音质）
func neteaseFillURLs(items []MusicItem) {
	if len(items) == 0 {
		return
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	urls, err := neteaseReq("/api/song/enhance/player/url", map[string]string{
		"ids": "[" + strings.Join(ids, ",") + "]", "br": "320000",
	})
	if err != nil {
		return
	}
	if data, ok := urls["data"].([]any); ok {
		type urlInfo struct {
			u  string
			br int
		}
		byID := map[string]urlInfo{}
		for _, d := range data {
			dm, _ := d.(map[string]any)
			id := fmt.Sprintf("%.0f", num(dm["id"]))
			byID[id] = urlInfo{u: str(dm["url"]), br: int(num(dm["br"]) / 1000)} // br=320000 → 320
		}
		for i := range items {
			if info, ok := byID[items[i].ID]; ok {
				items[i].URL = info.u
				if items[i].Quality == 0 && info.br > 0 {
					items[i].Quality = info.br
				}
			}
		}
	}
}

// filterPlayable 过滤无播放地址的曲目（无版权/会员专属拿不到直链）
func filterPlayable(items []MusicItem) []MusicItem {
	kept := items[:0]
	for _, it := range items {
		if it.URL != "" {
			kept = append(kept, it)
		}
	}
	return kept
}

// dedupKey 去重键（歌曲唯一标识；跨歌单同歌同键；直链按 URL）
func dedupKey(it MusicItem) string {
	if it.Source == "direct" {
		return "direct:" + it.URL
	}
	return it.Source + ":" + it.ID
}

func neteaseArtistsShort(sm map[string]any) string {
	if arts, ok := sm["ar"].([]any); ok {
		var names []string
		for _, a := range arts {
			am, _ := a.(map[string]any)
			names = append(names, str(am["name"]))
		}
		return strings.Join(names, " / ")
	}
	return ""
}

func neteaseCoverShort(sm map[string]any) string {
	if alb, ok := sm["al"].(map[string]any); ok {
		return str(alb["picUrl"])
	}
	return ""
}

func neteaseArtists(sm map[string]any) string {
	arts, _ := sm["artists"].([]any)
	var names []string
	for _, a := range arts {
		am, _ := a.(map[string]any)
		names = append(names, str(am["name"]))
	}
	return strings.Join(names, " / ")
}

func neteaseCover(sm map[string]any) string {
	if alb, ok := sm["album"].(map[string]any); ok {
		return str(alb["picUrl"])
	}
	return ""
}

// neteaseLyric 按需拉取歌词（LRC 文本）
// NetEaseLyricResult 歌词三件套（原文/翻译/音译）
type NetEaseLyricResult struct {
	Lrc     string `json:"lrc"`     // 原文 LRC
	Tlyric  string `json:"tlyric"`  // 翻译 LRC（中文）
	Romalrc string `json:"romalrc"` // 音译 LRC（罗马音/拼音）
}

func neteaseLyric(id string) NetEaseLyricResult {
	det, err := neteaseReq("/api/song/lyric", map[string]string{"id": id, "lv": "1", "tv": "1", "rv": "1"})
	if err != nil {
		return NetEaseLyricResult{}
	}
	var res NetEaseLyricResult
	if lrc, ok := det["lrc"].(map[string]any); ok {
		res.Lrc, _ = lrc["lyric"].(string)
	}
	if tl, ok := det["tlyric"].(map[string]any); ok {
		res.Tlyric, _ = tl["lyric"].(string)
	}
	if rm, ok := det["romalrc"].(map[string]any); ok {
		res.Romalrc, _ = rm["lyric"].(string)
	}
	return res
}

// ---------- 直链（mp3/m4a/aac 等直接播放） ----------

type directParser struct{}

var directExtRe = regexp.MustCompile(`(?i)\.(mp3|m4a|aac|wav|flac|ogg|opus)(\?|$)`)

func (p *directParser) Match(u string) bool {
	if strings.Contains(u, "music.163.com") {
		return false
	}
	return directExtRe.MatchString(u) || strings.HasPrefix(u, "http")
}

func (p *directParser) Parse(u string) ([]MusicItem, error) {
	return []MusicItem{{ID: "d" + fmt.Sprint(len(u)), Title: u, URL: u, Source: "direct"}}, nil
}

// ---------- 工具 ----------

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// musicFetch 拉取音频流（网易云直链可能仍校验 Referer，代理时带上）
func musicFetch(u string) (*http.Response, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if strings.Contains(u, "music.126.net") || strings.Contains(u, "music.163.com") {
		req.Header.Set("Referer", "https://music.163.com/")
	}
	return neteaseHTTP.Do(req)
}

// ---------- HTTP 处理器（列表/添加/删除/流代理/歌词） ----------

func (h *hub) handleMusicList(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(h.musicState())
}

func (h *hub) handleMusicAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		http.Error(w, "缺少链接", http.StatusBadRequest)
		return
	}
	items, err := parseMusicURL(body.URL)
	if err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(items) == 0 {
		http.Error(w, "解析结果为空（无可播放歌曲，可能无版权/会员专属）", http.StatusBadRequest)
		return
	}
	h.musicMu.Lock()
	h.musicList = append(h.musicList, items...)
	if h.musicIndex < 0 {
		h.musicIndex = 0
	}
	h.saveMusicStateLocked()
	h.musicMu.Unlock()
	h.musicBroadcast()
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": len(items)})
}

func (h *hub) handleMusicRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Index int `json:"index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.musicRemove(body.Index)
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleMusicRemovePlaylist 移除整个歌单或单曲分组（scope = playlistId / "single"）
func (h *hub) handleMusicRemovePlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Scope == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.musicRemovePlaylist(body.Scope)
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleMusicRefreshPlaylist 刷新歌单（按歌单 id 重新拉取最新歌曲并替换）
func (h *hub) handleMusicRefreshPlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		PlaylistID string `json:"playlistId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PlaylistID == "" {
		http.Error(w, "缺少歌单 id", http.StatusBadRequest)
		return
	}
	items, err := FetchPlaylistByID(body.PlaylistID)
	if err != nil || len(items) == 0 {
		http.Error(w, "歌单拉取失败或无可播放歌曲", http.StatusBadRequest)
		return
	}
	h.musicReplacePlaylist(body.PlaylistID, items)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": len(items)})
}

// handleMusicSearch 网易云搜索（q 关键字；type=1 歌曲 / 1000 歌单；返回列表无直链）
func (h *hub) handleMusicSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "缺少搜索关键字", http.StatusBadRequest)
		return
	}
	limit := 15
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	typ := r.URL.Query().Get("type")
	var items []MusicItem
	var err error
	if typ == "1000" {
		items, err = neteaseSearchPlaylist(q, limit)
	} else {
		items, err = neteaseSearch(q, limit)
	}
	if err != nil {
		http.Error(w, "搜索失败", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": typ, "list": items})
}

// handleMusicCover 封面代理（网易云封面 CDN 防盗链，浏览器直连 404；带 Referer 拉取转发）
// 封面 URL 有时效（持久化后过期）：失败时按 URL 匹配列表条目，重新拉取元数据更新封面后重试
func (h *hub) handleMusicCover(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		http.Error(w, "缺少 url", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		http.Error(w, "非法 url", http.StatusBadRequest)
		return
	}
	writeCover := func(fetchURL string) bool {
		resp, err := musicFetch(fetchURL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "image/jpeg"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return false
		}
		w.Write(data)
		return true
	}
	if writeCover(u) {
		return
	}
	// 源封面失效（404/403）：查找列表中的对应曲目，重拉元数据更新封面后重试一次
	// 去重（并发只刷一次）+ 节流（5 分钟内同 URL 不重复刷，防 renderMusicList 反复触发）
	h.coverRefreshMu.Lock()
	if h.coverRefreshing[u] {
		h.coverRefreshMu.Unlock()
		http.Error(w, "封面刷新中", http.StatusNotFound)
		return
	}
	if last, ok := h.coverRefreshTried[u]; ok && time.Now().UnixMilli()-last < 5*60*1000 {
		h.coverRefreshMu.Unlock()
		http.Error(w, "封面不可用", http.StatusNotFound)
		return
	}
	h.coverRefreshing[u] = true
	h.coverRefreshTried[u] = time.Now().UnixMilli()
	h.coverRefreshMu.Unlock()
	defer func() {
		h.coverRefreshMu.Lock()
		delete(h.coverRefreshing, u)
		h.coverRefreshMu.Unlock()
	}()
	h.musicMu.Lock()
	var id, matchKind string // matchKind: "song"=歌曲封面 / "playlist"=歌单封面
	for i := range h.musicList {
		if h.musicList[i].Cover == u {
			id, matchKind = h.musicList[i].ID, "song"
			break
		}
		if h.musicList[i].PlaylistCover == u {
			id, matchKind = h.musicList[i].ID, "playlist"
			break
		}
	}
	h.musicMu.Unlock()
	if id == "" {
		http.Error(w, "封面不可用", http.StatusNotFound)
		return
	}
	if matchKind == "playlist" {
		// 歌单封面：轻量拉歌单详情拿新封面（不拉全部曲目）
		newCover := ""
		if det, err := neteaseReq("/api/v6/playlist/detail", map[string]string{"id": id, "n": "1"}); err == nil {
			if pl, ok := det["playlist"].(map[string]any); ok {
				newCover = str(pl["coverImgUrl"])
			}
		}
		if newCover != "" && newCover != u {
			h.musicMu.Lock()
			for i := range h.musicList {
				if h.musicList[i].PlaylistID == id && h.musicList[i].PlaylistCover == u {
					h.musicList[i].PlaylistCover = newCover
				}
			}
			h.saveMusicStateLocked()
			h.musicMu.Unlock()
			if writeCover(newCover) {
				return
			}
		}
		// 刷新失败/无新封面：置空失效封面（前端不再请求，避免每次打开反复 404）
		h.musicMu.Lock()
		for i := range h.musicList {
			if h.musicList[i].PlaylistID == id && h.musicList[i].PlaylistCover == u {
				h.musicList[i].PlaylistCover = ""
			}
		}
		h.saveMusicStateLocked()
		h.musicMu.Unlock()
		http.Error(w, "封面不可用", http.StatusNotFound)
		return
	}
	// 歌曲封面：重拉歌曲元数据更新封面
	if items, err := neteaseSongs([]string{id}); err == nil && len(items) > 0 && items[0].Cover != "" {
		h.musicMu.Lock()
		for i := range h.musicList {
			if h.musicList[i].ID == id && h.musicList[i].Cover == u {
				h.musicList[i].Cover = items[0].Cover
				h.saveMusicStateLocked()
				break
			}
		}
		h.musicMu.Unlock()
		if writeCover(items[0].Cover) {
			return
		}
	}
	// 刷新失败：置空失效封面（防反复请求）
	h.musicMu.Lock()
	for i := range h.musicList {
		if h.musicList[i].ID == id && h.musicList[i].Cover == u {
			h.musicList[i].Cover = ""
			h.saveMusicStateLocked()
			break
		}
	}
	h.musicMu.Unlock()
	http.Error(w, "封面不可用", http.StatusNotFound)
}

// musicRefreshURL 重拉网易云直链（URL 过期时 stream 代理自动刷新）：更新列表 + 清旧缓存 + 持久化。
// 同曲目 10 分钟内不重复刷新（防接口故障时高频请求）。
func (h *hub) musicRefreshURL(id string) (string, error) {
	h.musicMu.Lock()
	if last, ok := h.musicURLRefreshed[id]; ok && time.Now().UnixMilli()-last < 10*60*1000 {
		h.musicMu.Unlock()
		return "", fmt.Errorf("刷新过于频繁")
	}
	h.musicMu.Unlock()
	items, err := neteaseSongs([]string{id})
	if err != nil || len(items) == 0 || items[0].URL == "" {
		return "", fmt.Errorf("重拉播放地址失败")
	}
	fresh := items[0].URL
	h.musicMu.Lock()
	h.musicURLRefreshed[id] = time.Now().UnixMilli()
	for i := range h.musicList {
		if h.musicList[i].ID == id && h.musicList[i].Source == "netease" {
			old := h.musicList[i].URL
			if old != fresh {
				delete(h.musicCache, old)
				for j, u := range h.musicCacheOrder {
					if u == old {
						h.musicCacheOrder = append(h.musicCacheOrder[:j], h.musicCacheOrder[j+1:]...)
						break
					}
				}
				h.musicList[i].URL = fresh
				h.saveMusicStateLocked()
			}
			break
		}
	}
	h.musicMu.Unlock()
	return fresh, nil
}

// handleMusicStream 音频流代理：内存缓存最近几首 + 浏览器 HTTP 缓存头
// streamTmpURL 临时直链流代理（预览试听；带防盗链 Referer + Range 透传支持拖动/时长）
func streamTmpURL(w http.ResponseWriter, r *http.Request, u string) {
	fetch := func() (*http.Response, error) {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		if strings.Contains(u, "music.126.net") || strings.Contains(u, "music.163.com") {
			req.Header.Set("Referer", "https://music.163.com/")
		}
		if rng := r.Header.Get("Range"); rng != "" { // 浏览器拖动进度条 → 透传 Range
			req.Header.Set("Range", rng)
		}
		return neteaseHTTP.Do(req)
	}
	resp, err := fetch()
	if err != nil {
		http.Error(w, "拉取音频失败", http.StatusBadGateway)
		return
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		defer resp.Body.Close()
		// 透传关键响应头（时长/拖动需要）
		for _, h := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	resp.Body.Close()
	// 失败重试一次（瞬时抖动）
	resp2, err := fetch()
	if err != nil {
		http.Error(w, "拉取音频失败", http.StatusBadGateway)
		return
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK && resp2.StatusCode != http.StatusPartialContent {
		http.Error(w, "音频源返回错误", http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range"} {
		if v := resp2.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp2.StatusCode)
	io.Copy(w, resp2.Body)
}

// handleMusicPreview 临时预览（不加入播放列表）：type=song → 单曲（含直链）；type=playlist → 歌单曲目
func (h *hub) handleMusicPreview(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}
	var items []MusicItem
	var err error
	if typ == "playlist" {
		items, err = FetchPlaylistByID(id)
	} else {
		items, err = neteaseSongs([]string{id})
	}
	if err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(items) == 0 {
		http.Error(w, "无曲目（可能无版权/会员专属）", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "list": items})
}

// parseRange 解析 Range 头（bytes=start-end / start- / -suffix；单段）
func parseRange(h string, size int) (int, int) {
	if !strings.HasPrefix(h, "bytes=") || size <= 0 {
		return 0, size - 1
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	parts := strings.SplitN(spec, "-", 2)
	start, end := 0, size-1
	if parts[0] != "" {
		if s, err := strconv.Atoi(parts[0]); err == nil {
			start = s
		}
	}
	if len(parts) > 1 && parts[1] != "" {
		if e, err := strconv.Atoi(parts[1]); err == nil {
			end = e
		}
	} else if parts[0] == "" && len(parts) > 1 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			start = size - n // 后缀长度
		}
	}
	if start < 0 {
		start = 0
	}
	if end >= size {
		end = size - 1
	}
	if end < start {
		end = start
	}
	return start, end
}

// writeStreamData 带 Range 支持的数据响应（206 分段 / 200 全量）
func writeStreamData(w http.ResponseWriter, r *http.Request, ct string, data []byte) {
	size := len(data)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if rng := r.Header.Get("Range"); rng != "" && size > 0 {
		start, end := parseRange(rng, size)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Write(data)
}

func (h *hub) handleMusicStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	tmpURL := r.URL.Query().Get("url")
	h.musicMu.Lock()
	var item *MusicItem
	if id != "" {
		for i := range h.musicList {
			if h.musicList[i].ID == id {
				item = &h.musicList[i]
				break
			}
		}
	}
	h.musicMu.Unlock()
	// 临时直链（预览试听：不依赖播放列表）
	if tmpURL != "" {
		if strings.HasPrefix(tmpURL, "http://") || strings.HasPrefix(tmpURL, "https://") {
			streamTmpURL(w, r, tmpURL)
			return
		}
	}
	if item == nil || item.URL == "" {
		http.Error(w, "曲目不存在或无播放地址", http.StatusNotFound)
		return
	}
	// 内存缓存（最近 musicCacheMax 首）
	h.musicMu.Lock()
	if b, ok := h.musicCache[item.URL]; ok {
		h.musicMu.Unlock()
		writeStreamData(w, r, "audio/mpeg", b)
		return
	}
	h.musicMu.Unlock()
	// 拉取源（可缓存到内存）
	resp, err := musicFetch(item.URL)
	if (err != nil || resp.StatusCode != http.StatusOK) && item.Source == "netease" {
		// 网易云直链有时效（带时间戳签名）：持久化列表重启后 URL 过期 → 自动刷新重试一次
		if resp != nil {
			resp.Body.Close()
		}
		if fresh, rerr := h.musicRefreshURL(item.ID); rerr == nil {
			resp, err = musicFetch(fresh)
		}
	}
	if err != nil {
		http.Error(w, "拉取音频失败（链接可能已失效，请重新添加）", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "音频源返回错误", http.StatusBadGateway)
		return
	}
	// 限制读入内存（单首上限 32MB，防撑爆；超过则流式转发带 Range 透传）
	const maxCache = 32 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCache+1))
	if err != nil {
		http.Error(w, "读取失败", http.StatusInternalServerError)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "audio/mpeg"
	}
	if len(data) > maxCache {
		// 超限：释放已读数据，改为流式转发（Range 透传支持拖动）
		streamTmpURL(w, r, item.URL)
		return
	}
	if len(data) <= maxCache {
		// 入内存缓存（LRU）
		h.musicMu.Lock()
		if _, ok := h.musicCache[item.URL]; !ok {
			h.musicCache[item.URL] = data
			h.musicCacheOrder = append(h.musicCacheOrder, item.URL)
			if len(h.musicCacheOrder) > musicCacheMax {
				old := h.musicCacheOrder[0]
				h.musicCacheOrder = h.musicCacheOrder[1:]
				delete(h.musicCache, old)
			}
		}
		h.musicMu.Unlock()
	}
	writeStreamData(w, r, ct, data)
}

// handleMusicLyric 歌词（原文+翻译+音译 JSON）
func (h *hub) handleMusicLyric(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	h.musicMu.Lock()
	var item *MusicItem
	for i := range h.musicList {
		if h.musicList[i].ID == id {
			item = &h.musicList[i]
			break
		}
	}
	h.musicMu.Unlock()
	if item == nil || item.Source != "netease" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	lrc := neteaseLyric(item.ID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(lrc)
}
