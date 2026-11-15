package sse

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	pkgsse "github.com/oekit/pkg/sse"
)

// 本文件是 pkg/sse 内核（Hub/Event/ReplayFunc/「订阅先于回放」去重）的 Hertz
// 形态装配（#302）：实时面与取数面直接复用 github.com/oekit/pkg/sse（传输
// 无关），本包只补 Hertz 侧的两处传输差异——游标读取与帧写出。游标解析、
// 帧序列化、心跳与慢消费者自愈语义与 pkg/sse 逐条对齐（帧字节一致，见各处
// 注记）；地板保持 pkg v0.50.0（其 sse 包未导出 parseCursor/frame，故此处
// 有同口径的本地实现，升级若导出后可收敛）。
//
// 用法（端点接入 ≤10 行）：
//
//	hub := pkgsse.NewHub()
//	replay := func(after uint64) []pkgsse.Event { return db.EventsAfter(after) }
//	resp := sse.NewResponder(hub, replay)
//	h.GET("/api/v1/task/runs/stream", resp.Serve)
//
//	// 事件落库成功后广播实时面（ID 由宿主单调递增分配）：
//	hub.Emit(pkgsse.Event{ID: next, Type: "run.step", Data: string(json)})

// defaultHeartbeatComment 心跳注释帧缺省文案（与 pkg/sse 缺省一致）。
const defaultHeartbeatComment = ": keepalive"

// Responder SSE 游标回放应答器（Hertz 形态）：一条连接承担「响应头四件套
// → 订阅先于回放 → 按事件 ID 幂等跳帧 → 心跳注释帧保活 → 断开即退订」
// 全套语义，与 pkg/sse.Responder 同一契约、同一内核（pkg/sse.Source +
// pkg/sse.ReplayFunc），差异仅在传输适配（#302）。
//
// 断开检测口径：Hertz HTTP/1 引擎传给 handler 的 ctx 不随客户端断开取消，
// 断开由**写失败**发现（每帧/每次心跳都 flush，死连接在下一个心跳间隔内
// 暴露）——defer cancel 退订，Hub 不留死订阅。
type Responder struct {
	src       pkgsse.Source
	replay    pkgsse.ReplayFunc
	heartbeat time.Duration
	comment   string
}

// ResponderOption Responder 的构造选项（仓规禁裸 Option）。
type ResponderOption func(*Responder)

// WithHeartbeat 覆盖心跳间隔（缺省 [pkgsse.DefaultHeartbeat] 15s）。非正数
// panic（构造期 fail-fast，仓规总则①）。
func WithHeartbeat(d time.Duration) ResponderOption {
	return func(r *Responder) {
		if d <= 0 {
			panic("sse: WithHeartbeat 间隔必须为正数（fail-fast，仓规总则①）")
		}
		r.heartbeat = d
	}
}

// WithHeartbeatComment 覆盖心跳注释帧文案（缺省 `: keepalive`）。必须是
// 冒号开头的单行注释——SSE 里冒号开头才是客户端忽略的注释帧，非注释
// 形态会被 EventSource 当事件解析。非法值 panic（构造期 fail-fast）。
func WithHeartbeatComment(comment string) ResponderOption {
	return func(r *Responder) {
		if !validComment(comment) {
			panic("sse: WithHeartbeatComment 必须是冒号开头且不含换行的注释（fail-fast，仓规总则①）")
		}
		r.comment = comment
	}
}

// NewResponder 建立应答器。src 为实时面（[pkgsse.Hub] 或宿主自有
// [pkgsse.Source] 实现），replay 为取数面；二者 nil panic（构造期 fail-fast，
// 仓规总则①）。
func NewResponder(src pkgsse.Source, replay pkgsse.ReplayFunc, opts ...ResponderOption) *Responder {
	if src == nil {
		panic("sse: NewResponder 缺实时面 Source（fail-fast，仓规总则①）")
	}
	if replay == nil {
		panic("sse: NewResponder 缺游标回放 ReplayFunc（fail-fast，仓规总则①）")
	}
	r := &Responder{
		src:       src,
		replay:    replay,
		heartbeat: pkgsse.DefaultHeartbeat,
		comment:   defaultHeartbeatComment,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Serve 应答一条 SSE 流（app.HandlerFunc 形态，直接挂路由：
// h.GET("/stream", resp.Serve)）。游标取 `Last-Event-ID` 请求头（EventSource
// 重连自动携带；缺省 0 = 全量回放；非法值 400，fail-closed——非法游标多半
// 是客户端 bug，静默按 0 全量回放会放大成一次昂贵的误回放）。
//
// 与 pkg/sse.Responder 的口径差异只有一处：Hertz 的流式 Writer 无「只冲头
// 不带体」的 API，响应头随首帧（回放首帧或首个心跳）一并发出。
func (r *Responder) Serve(ctx context.Context, c *app.RequestContext) {
	cursor, ok := parseCursor(string(c.GetHeader("Last-Event-ID")))
	if !ok {
		c.String(consts.StatusBadRequest, "Last-Event-ID 必须是无符号整数\n")
		return
	}

	// SSE 应答头四件套（与 pkg/sse.setHeaders 同口径；官方 NewStream 随后
	// 复设其中 Content-Type/Cache-Control 为同值，其余两件由此处补齐）。
	h := &c.Response.Header
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Response.SetStatusCode(consts.StatusOK)

	stream := NewStream(c)
	defer func() { _ = stream.Close() }()

	// 订阅先于回放（核心不变量，与 pkg/sse 同）：先拿实时面通道，再查
	// 窗口——缝隙归零，重叠部分靠幂等跳帧去重。
	frames, cancel := r.src.Subscribe()
	defer cancel()

	last := cursor
	for _, e := range r.replay(last) {
		if e.ID <= last {
			continue // 回放窗口内的乱序/重叠帧跳过（宿主契约的防御性兜底）
		}
		if err := writeEvent(stream, e); err != nil {
			return
		}
		last = e.ID
	}

	// 心跳注释帧：空闲保活，防中间层掐流（语义见 pkg/sse 包文档）。
	ticker := time.NewTicker(r.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return // 引擎包装了可取消 ctx 时提前收尾（原生引擎不触发）
		case <-ticker.C:
			if err := writeHeartbeat(stream, r.comment); err != nil {
				return
			}
		case e, ok := <-frames:
			if !ok {
				return // Hub Close 或慢消费者掉队断流：客户端带游标重连补窗
			}
			if e.ID <= last {
				continue // 订阅/回放重叠去重：同 ID 只写一次
			}
			if err := writeEvent(stream, e); err != nil {
				return
			}
			last = e.ID
		}
	}
}

// writeEvent 按 SSE 文本协议写出一帧：id/event/data 各一行、数据多行拆分、
// 空行结尾（事件分帧符）——经官方 Writer 产出的帧字节与 pkg/sse Event.frame
// 一致。空 Data 传单字节 '\n'（#229 口径）：上游 Writer 对零长度 data 一个
// 字节都不写（无 data 行 ⇒ 事件不被派发），'\n' 恰好拆出一行空值 data: 字段，
// 与 pkg/sse 对空 Data 写 "data: " 行的形态对齐。
func writeEvent(s *Stream, e pkgsse.Event) error {
	data := []byte(e.Data)
	if len(data) == 0 {
		data = []byte{'\n'}
	}
	return s.SendRaw(strconv.FormatUint(e.ID, 10), e.Type, data)
}

// writeHeartbeat 写一帧心跳注释。官方 Writer.WriteComment 自带冒号前缀且
// 不写分帧空行——先写注释行（入参去前导冒号），再补一个空事件帧（"\n"）
// 作分帧符，线上字节与 pkg/sse 的 `comment + "\n\n"` 一致。
func writeHeartbeat(s *Stream, comment string) error {
	if err := s.WriteComment(strings.TrimPrefix(comment, ":")); err != nil {
		return err
	}
	return s.Write(&Event{})
}

// parseCursor 解析 Last-Event-ID 游标：空 = 0（全量回放）；非合法无符号
// 整数 = 非法（调用方按 400 拒绝）。与 pkg/sse 的 parseCursor 同口径（v0.50.0
// 未导出，本地同实现；升级若导出后可直接换用）。
func parseCursor(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// validComment 心跳注释合法性：冒号开头、单行（SSE 注释帧形态约束）。
func validComment(s string) bool {
	return len(s) > 0 && s[0] == ':' && !containsNewline(s)
}

func containsNewline(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return true
		}
	}
	return false
}
