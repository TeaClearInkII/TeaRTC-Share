# TeaShare 屏幕共享 — 实施计划

多对多屏幕共享：兼容"不安装服务"（纯浏览器，SFU 中继）与"安装服务"（多节点智能组网）两种形态。

## 总体架构

```
共享者A浏览器 ──推流pc──> Go节点(SFU) ──拉流pc──> 观看者B浏览器
共享者C浏览器 ──推流pc──>             └──拉流pc──> 观看者D浏览器
节点A ══(节点间WebRTC)══ 节点B   （二期）
```

- 浏览器用户：只连自己加入的节点，推流/拉流都经该节点（纯 SFU）
- 装服务用户：节点间自动发现互联（组网链接触发），级联转发，智能选路
- 保留：TURN TCP 中继（远程 UDP 被运营商丢弃兜底）、自签 CA 引导、防火墙命令

## 一期：单节点 SFU + simulcast 聚焦降级（独立可用）

### 1. 信令重构（main.go）
- 对等模型，去掉 sharer/viewer 角色
- `hello{name}` → `welcome{id, name, color, peers[], streams[]}`（全量快照）
- 广播：`peer-joined` / `peer-left` / `stream-added{ownerId,streamId}` / `stream-removed{ownerId,streamId}`
- 推流：`publish-offer/answer`（浏览器发 offer，sendonly + simulcast 三档）
- 拉流：`subscribe-offer/answer`（服务器发 offer，AddTrack 全部流；流增减触发重协商）
- 转发：`candidate`（带 pc 类型区分 publish/subscribe）、`viewer-config{streamId,tier}`（换层）、`ping/pong`
- 保留：TURN、CA、防火墙命令、昵称/颜色分配

### 2. SFU 核心（pion/webrtc）
- 推流 pc：`OnTrack` 按 `RID` 区分 simulcast 三层 → 每层 `TrackLocalStaticRTP` 转发池
- 拉流 pc：每流一个 sender，档位切换用 `sender.ReplaceTrack` 换层（免重协商）
- 流增减：`AddTrack`/`RemoveTrack` + 重协商（re-offer）
- RTCP：PLI/关键帧转发（查 pion API，若无回调则依赖浏览器关键帧间隔 ≤2s）
- 无人订阅的层不转发（省带宽）

### 3. 前端重构（web/index.html）
- 移除角色模式，统一圆角卡片网格
- 我的卡片：预览 + 开始/停止共享 + 分辨率/帧率/码率配置 + 发送统计
- 每用户卡片：颜色圆点 + 昵称 + 共享状态徽章 + 视频 + 统计小字（分辨率/fps/RTT/丢包）
- 聚焦机制：点击卡片放大 → 该流高码率层，其余自动低层（真省带宽）
- 档位：按观看者×共享者独立（低/中/高）
- 保留：CA 引导、复制命令、连接详情

### 4. 验收
多浏览器同网：多人加入、多路共享、聚焦降级带宽变化、停止共享实时移除、档位切换

## 二期：节点互联智能组网

### 1. 自动发现（带外机制）
```
A 页面探测本机服务: fetch http://127.0.0.1:7553/api/ping（CORS，仅返回运行标识）
发现本机有服务 → 提示生成组网链接: https://127.0.0.1:7553/?join=<A服务地址>
B 用户本机打开该链接 → B 页面调 B 服务 /api/join?remote=<A> → B 服务主动连 A 服务
→ 同步用户/流表 → 重新智能组网
```
- 安全：仅本机页面触发；节点间共享密钥认证（join 时交换）

### 2. 节点互联
- 节点间 WS（内部协议 + token）：注册、用户/流表增量同步、心跳、断线重连
- 节点间 WebRTC（pion↔pion）：级联转发通道（复用 TrackLocal 转发逻辑）
- 流路由表：`(节点, 用户, 流)` 三级索引（一期就按此建模，二期只加远端分支）

### 3. 智能路径
- 本地优先 → 直连节点 1 跳 → 多跳最少化
- 跨节点订阅：观看节点直连源节点拉流；无人观看自动拆除级联
- 节点掉线：标记离线 + 观看者自动重路由

### 4. P2P 直连优化（同节点用户间）
- 网络可达时浏览器直连，省节点带宽（可选切换）
