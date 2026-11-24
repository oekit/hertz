package sse

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	pkgsse "github.com/oekit/pkg/sse"
)

// Responder 回归（#302）：Hertz 端点经 pkg/sse 内核（Hub/ReplayFunc/游标
// 去重）装配后的契约——与 pkg/sse/sse_test.go 同构的用例集，帧字节逐字节
// 对齐；流式读取走 net/http 客户端（Hertz server 即 HTTP/1.1 服务）。

// sseClient 直连测试服务器的流式 HTTP 客户端（Hertz client 的 Do 读到响应
// 结束，做不了逐帧断言；SSE 端点本就是标准 HTTP/1.1 chunked 流）。
var sseClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// connectH 建立一条 SSE 连接，返回响应与可取消的请求上下文。
// ctx 超时是读护栏：测试逻辑卡死时读取报错而非挂死整个测试。
func connectH(t *testing.T, addr string, header http.Header) (*http.Response, context.CancelFunc, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("构造请求: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := sseClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("发起请求: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("状态码 = %d，body = %q", resp.StatusCode, body)
	}
	return resp, cancel, bufio.NewReader(resp.Body)
}

// readFrame 读完一帧（到空行）。
func readFrame(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读帧: %v（已收 %q）", err, sb.String())
		}
		sb.WriteString(line)
		if line == "\n" {
			return sb.String()
		}
	}
}

// expectFrame 期望的事件帧形态（与 pkg/sse Event.frame 序列化对齐）。
func expectFrame(id uint64, typ, data string) string {
	var sb strings.Builder
	sb.WriteString("id: " + strconv.FormatUint(id, 10) + "\n")
	if typ != "" {
		sb.WriteString("event: " + typ + "\n")
	}
	for _, line := range strings.Split(data, "\n") {
		sb.WriteString("data: " + line + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// TestResponder_ReplayWindowOrderAndDedup 契约（复用 pkg/sse
// TestReplayWindowOrderAndDedup 的场景）：游标窗口回放序 + 订阅后不重复。
// 「订阅先于回放」下，回放窗口与实时广播的重叠帧（ID 6）恰好写一次；
// 重复广播的同 ID 帧与旧游标帧跳过；订阅前的广播不进流。空 Data 帧产出
// 空值 data 行（#229 口径与 pkg/sse 对齐，事件可派发）。
func TestResponder_ReplayWindowOrderAndDedup(t *testing.T) {
	hub := pkgsse.NewHub()
	afterCh := make(chan uint64, 1)
	replay := func(after uint64) []pkgsse.Event {
		afterCh <- after
		hub.Emit(pkgsse.Event{ID: 6, Data: "during"}) // 模拟回放进行中的实时广播（与窗口尾重叠）
		return []pkgsse.Event{
			{ID: 3, Type: "run.step", Data: "a"},
			{ID: 4, Data: "b1\nb2"},
			{ID: 5, Data: "c"},
			{ID: 6, Data: "during"},
		}
	}
	addr, cleanup := startSSEServer(t, NewResponder(hub, replay, WithHeartbeat(time.Hour)).Serve)
	defer cleanup()

	hub.Emit(pkgsse.Event{ID: 2, Data: "before"}) // 订阅建立前广播：不进任何流

	resp, cancel, br := connectH(t, addr, http.Header{"Last-Event-ID": {"2"}})
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()

	if got := <-afterCh; got != 2 {
		t.Fatalf("replay 收到 after = %d，应为游标 2", got)
	}

	want := []string{
		expectFrame(3, "run.step", "a"),
		expectFrame(4, "", "b1\nb2"),
		expectFrame(5, "", "c"),
		expectFrame(6, "", "during"), // 回放写入一次
	}
	for i, w := range want {
		if got := readFrame(t, br); got != w {
			t.Fatalf("第 %d 帧 = %q，应为 %q", i+1, got, w)
		}
	}

	hub.Emit(pkgsse.Event{ID: 7, Data: "live"})  // 回放结束后的实时帧
	hub.Emit(pkgsse.Event{ID: 6, Data: "dup"})   // 同 ID 重复广播：必须跳过
	hub.Emit(pkgsse.Event{ID: 8, Data: "live2"}) // 后续实时帧不受影响
	hub.Emit(pkgsse.Event{ID: 2, Data: "stale"}) // 旧游标重放：必须跳过
	hub.Emit(pkgsse.Event{ID: 9, Data: ""})      // 空 Data 帧：仍产出空值 data 行
	if got := readFrame(t, br); got != expectFrame(7, "", "live") {
		t.Fatalf("实时帧 = %q，应为 %q", got, expectFrame(7, "", "live"))
	}
	if got := readFrame(t, br); got != expectFrame(8, "", "live2") {
		t.Fatalf("跳帧后的实时帧 = %q，应为 %q", got, expectFrame(8, "", "live2"))
	}
	if got := readFrame(t, br); got != expectFrame(9, "", "") {
		t.Fatalf("空数据帧 = %q，应为 %q", got, expectFrame(9, "", ""))
	}

	if err := hub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := br.Peek(1); err != io.EOF {
		t.Fatalf("Hub.Close 后流应结束（io.EOF），got err = %v", err)
	}
}

// TestResponder_FullReplayWithoutCursor 无 Last-Event-ID 视为游标 0（全量回放）。
func TestResponder_FullReplayWithoutCursor(t *testing.T) {
	hub := pkgsse.NewHub()
	afterCh := make(chan uint64, 1)
	replay := func(a uint64) []pkgsse.Event {
		afterCh <- a
		return []pkgsse.Event{{ID: 1, Data: "x"}}
	}
	addr, cleanup := startSSEServer(t, NewResponder(hub, replay, WithHeartbeat(time.Hour)).Serve)
	defer cleanup()

	resp, cancel, br := connectH(t, addr, nil)
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()
	if got := readFrame(t, br); got != expectFrame(1, "", "x") {
		t.Fatalf("回放帧 = %q，应为 %q", got, expectFrame(1, "", "x"))
	}
	if got := <-afterCh; got != 0 {
		t.Fatalf("无游标应全量回放（after=0），got %d", got)
	}
}

// TestResponder_BadCursorRejected 非法 Last-Event-ID 按 400 拒绝
// （fail-closed），拒绝路径不留订阅。
func TestResponder_BadCursorRejected(t *testing.T) {
	hub := pkgsse.NewHub()
	addr, cleanup := startSSEServer(t, NewResponder(hub, func(uint64) []pkgsse.Event { return nil }).Serve)
	defer cleanup()

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/stream", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Last-Event-ID", "abc")
	resp, err := sseClient.Do(req)
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("非法游标应 400，got %d（body=%q）", resp.StatusCode, body)
	}
	if n := hub.Subscribers(); n != 0 {
		t.Fatalf("拒绝路径不应留下订阅，Subscribers = %d", n)
	}
}

// TestResponder_HeartbeatFrameShape 契约：心跳注释帧字节与 pkg/sse 一致
// （": keepalive\n\n"——注释行 + 空行分帧），空闲期按间隔持续发出。
func TestResponder_HeartbeatFrameShape(t *testing.T) {
	hub := pkgsse.NewHub()
	addr, cleanup := startSSEServer(t, NewResponder(hub, func(uint64) []pkgsse.Event { return nil },
		WithHeartbeat(20*time.Millisecond)).Serve)
	defer cleanup()

	resp, cancel, br := connectH(t, addr, nil)
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()

	for i := 0; i < 3; i++ {
		if got := readFrame(t, br); got != ": keepalive\n\n" {
			t.Fatalf("第 %d 个心跳帧 = %q，应为 \": keepalive\\n\\n\"", i+1, got)
		}
	}
}

// TestResponder_HeartbeatCommentOverride 心跳文案可换（`: ping` 形态等价
// 合法），分帧空行不变。
func TestResponder_HeartbeatCommentOverride(t *testing.T) {
	hub := pkgsse.NewHub()
	addr, cleanup := startSSEServer(t, NewResponder(hub, func(uint64) []pkgsse.Event { return nil },
		WithHeartbeat(15*time.Millisecond), WithHeartbeatComment(": ping")).Serve)
	defer cleanup()

	resp, cancel, br := connectH(t, addr, nil)
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()

	if got := readFrame(t, br); got != ": ping\n\n" {
		t.Fatalf("心跳帧 = %q，应为 \": ping\\n\\n\"", got)
	}
}

// TestResponder_ResponseHeaders 契约：SSE 应答头四件套齐备且状态 200。
func TestResponder_ResponseHeaders(t *testing.T) {
	hub := pkgsse.NewHub()
	replay := func(uint64) []pkgsse.Event { return []pkgsse.Event{{ID: 1, Data: "x"}} }
	addr, cleanup := startSSEServer(t, NewResponder(hub, replay, WithHeartbeat(time.Hour)).Serve)
	defer cleanup()

	resp, cancel, _ := connectH(t, addr, nil)
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()

	want := map[string]string{
		"Content-Type":      "text/event-stream; charset=utf-8",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Fatalf("响应头 %s = %q，应为 %q", k, got, v)
		}
	}
}

// TestResponder_ClientDisconnectCleanup 契约：客户端断开后订阅被清理
// （写失败发现断开，Hub 不留死订阅），断开后的广播安全无害。
func TestResponder_ClientDisconnectCleanup(t *testing.T) {
	hub := pkgsse.NewHub()
	addr, cleanup := startSSEServer(t, NewResponder(hub, func(uint64) []pkgsse.Event { return nil },
		WithHeartbeat(15*time.Millisecond)).Serve)
	defer cleanup()

	resp, cancel, br := connectH(t, addr, nil)
	if got := readFrame(t, br); got != ": keepalive\n\n" {
		t.Fatalf("心跳帧 = %q", got)
	}
	cancel() // 客户端断开
	_ = resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Subscribers() == 0 {
			hub.Emit(pkgsse.Event{ID: 1, Data: "x"}) // 断开后广播：不得 panic
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("断开后订阅未清理，Subscribers = %d", hub.Subscribers())
}

// TestResponder_HubCloseEndsStreams Hub.Close 使在订流有序结束（io.EOF），
// Close 幂等、Close 后 Subscribe 返回已关闭通道（pkg/sse Hub 契约在 Hertz
// 装配下不变）。
func TestResponder_HubCloseEndsStreams(t *testing.T) {
	hub := pkgsse.NewHub()
	addr, cleanup := startSSEServer(t, NewResponder(hub, func(uint64) []pkgsse.Event { return nil },
		WithHeartbeat(15*time.Millisecond)).Serve)
	defer cleanup()

	resp, cancel, br := connectH(t, addr, nil)
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()
	if got := readFrame(t, br); got != ": keepalive\n\n" {
		t.Fatalf("心跳帧 = %q", got)
	}

	if err := hub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := hub.Close(context.Background()); err != nil {
		t.Fatalf("Close 幂等: %v", err)
	}
	if _, err := br.Peek(1); err != io.EOF {
		t.Fatalf("Close 后流应结束（io.EOF），got err = %v", err)
	}
	ch, cancelSub := hub.Subscribe()
	defer cancelSub()
	if _, ok := <-ch; ok {
		t.Fatal("Close 后 Subscribe 应返回已关闭通道")
	}
}

// TestResponder_LaggardDisconnect 慢消费者缓冲满即断流：缓冲内帧仍可读
// 完、掉队帧以断流收场（丢弃帧不得静默——客户端带游标重连由回放补窗），
// 掉队者从在订表摘除。两帧在回放回调内同步广播（此刻 handler 必在回放
// 循环、尚未开始消费实时通道），缓冲 1 ⇒ 掉队断流是确定性的。
func TestResponder_LaggardDisconnect(t *testing.T) {
	hub := pkgsse.NewHub(pkgsse.WithSubscriberBuffer(1))
	replay := func(uint64) []pkgsse.Event {
		hub.Emit(pkgsse.Event{ID: 2, Data: "buffered"}) // 缓冲内
		hub.Emit(pkgsse.Event{ID: 3, Data: "lagged"})   // 缓冲满 = 掉队：断流
		return []pkgsse.Event{{ID: 1, Data: "replay"}}
	}
	addr, cleanup := startSSEServer(t, NewResponder(hub, replay, WithHeartbeat(time.Hour)).Serve)
	defer cleanup()

	resp, cancel, br := connectH(t, addr, nil)
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()

	if got := readFrame(t, br); got != expectFrame(1, "", "replay") {
		t.Fatalf("回放帧 = %q，应为 %q", got, expectFrame(1, "", "replay"))
	}
	if got := readFrame(t, br); got != expectFrame(2, "", "buffered") {
		t.Fatalf("缓冲内帧 = %q，应为 %q", got, expectFrame(2, "", "buffered"))
	}
	if _, err := br.Peek(1); err != io.EOF {
		t.Fatalf("掉队后流应关闭（io.EOF），got err = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Subscribers() == 0 {
			hub.Emit(pkgsse.Event{ID: 4, Data: "x"}) // 对已断流广播：不得 panic
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("掉队者应被摘除，Subscribers = %d", hub.Subscribers())
}

// TestResponder_LaggardReconnectResumesByCursor 掉队断流后的自愈闭环：客户端
// 带最后收到的游标重连，ReplayFunc 补齐被丢弃的窗口（与 pkg/sse 包文档
// 「慢消费者自愈」一致）。
func TestResponder_LaggardReconnectResumesByCursor(t *testing.T) {
	hub := pkgsse.NewHub(pkgsse.WithSubscriberBuffer(1))
	replay := func(after uint64) []pkgsse.Event {
		switch after {
		case 0:
			hub.Emit(pkgsse.Event{ID: 2, Data: "buffered"}) // 缓冲内
			hub.Emit(pkgsse.Event{ID: 3, Data: "lagged"})   // 缓冲满 = 掉队：断流
			return []pkgsse.Event{{ID: 1, Data: "replay"}}
		case 2:
			return []pkgsse.Event{{ID: 3, Data: "lagged"}} // 重连补窗
		default:
			return nil
		}
	}
	addr, cleanup := startSSEServer(t, NewResponder(hub, replay, WithHeartbeat(time.Hour)).Serve)
	defer cleanup()
	defer func() { _ = hub.Close(context.Background()) }()

	resp, cancel, br := connectH(t, addr, nil)
	if got := readFrame(t, br); got != expectFrame(1, "", "replay") {
		t.Fatalf("回放帧 = %q，应为 %q", got, expectFrame(1, "", "replay"))
	}
	if got := readFrame(t, br); got != expectFrame(2, "", "buffered") {
		t.Fatalf("缓冲内帧 = %q，应为 %q", got, expectFrame(2, "", "buffered"))
	}
	if _, err := br.Peek(1); err != io.EOF {
		t.Fatalf("掉队后流应关闭（io.EOF），got err = %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	// 等掉队订阅从在订表摘除（defer cancel 已随 handler 返回执行）。
	deadline := time.Now().Add(2 * time.Second)
	for hub.Subscribers() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// 带最后收到的游标 2 重连：回放补齐被丢弃的 3。
	resp2, cancel2, br2 := connectH(t, addr, http.Header{"Last-Event-ID": {"2"}})
	defer func() {
		cancel2()
		_ = resp2.Body.Close()
	}()
	if got := readFrame(t, br2); got != expectFrame(3, "", "lagged") {
		t.Fatalf("重连回放帧 = %q，应为 %q", got, expectFrame(3, "", "lagged"))
	}
}

// TestResponderConstructorFailFast 构造期非法值 fail-fast（仓规总则①）。
func TestResponderConstructorFailFast(t *testing.T) {
	hub := pkgsse.NewHub()
	replay := pkgsse.ReplayFunc(func(uint64) []pkgsse.Event { return nil })
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s 应 panic（fail-fast）", name)
			}
		}()
		f()
	}
	mustPanic("nil Source", func() { _ = NewResponder(nil, replay) })
	mustPanic("nil ReplayFunc", func() { _ = NewResponder(hub, nil) })
	mustPanic("心跳非正数", func() { _ = NewResponder(hub, replay, WithHeartbeat(0)) })
	mustPanic("心跳负数", func() { _ = NewResponder(hub, replay, WithHeartbeat(-time.Second)) })
	mustPanic("注释无冒号", func() { _ = NewResponder(hub, replay, WithHeartbeatComment("ping")) })
	mustPanic("注释含换行", func() { _ = NewResponder(hub, replay, WithHeartbeatComment(": a\nb")) })
}
