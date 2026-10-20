package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/oekit/hertz/health"
)

// 导出的响应面必须与实际 JSON 逐字段对应——消费方（包外）用 health.HealthResponse
// 直接解码 /health 的真实响应体，不该再手写信封结构体去猜内层形态。
//
// 本文件刻意用外部测试包（package health_test）：只有包外消费方能引用的符号，
// 才是真正的「导出面」。内包测试引用未导出符号也编译得过，证明不了这一点。
//
// 断言分两层，缺一不可：
//  1. 原始 JSON 的键名/键集合（字面契约）——防止 json tag 写错时
//     「生产端与消费端同一个结构体」互相抵消、测试假绿；
//  2. 用导出的类型解码后的值——证明导出的字段真的映射到了这些键。

// performHealth 跑一次 /health，返回状态码与响应体。
func performHealth(t *testing.T, opts ...health.HealthOption) (int, []byte) {
	t.Helper()
	handler := health.NewHealthHandler(opts...)
	c := app.NewContext(0)
	handler(context.Background(), c)
	return c.Response.StatusCode(), c.Response.Body()
}

// decodeHealthResponse 用导出的 health.HealthResponse 解码响应体。
func decodeHealthResponse(t *testing.T, body []byte) health.HealthResponse {
	t.Helper()
	var resp health.HealthResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal into health.HealthResponse: %v (body=%s)", err, body)
	}
	return resp
}

// assertJSONKeys 断言 raw JSON 对象的键集合恰好是 want（顺序无关）。
// 键名写错（json tag 改错/字段改名）会在这里变红。
func assertJSONKeys(t *testing.T, label string, raw []byte, want ...string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s is not a JSON object: %v (raw=%s)", label, err, raw)
	}
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Errorf("%s keys = %v, want %v (raw=%s)", label, got, wantSorted, raw)
	}
}

// assertEnvelopeShape 断言响应体的字面键契约：
// 顶层恰好 {code,data,message,trace_id}，data 恰好 {checks,status}。
func assertEnvelopeShape(t *testing.T, body []byte) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal top-level: %v (body=%s)", err, body)
	}
	assertJSONKeys(t, "top-level", body, "code", "data", "message", "trace_id")
	rawData, ok := top["data"]
	if !ok {
		t.Fatalf("top-level missing \"data\" (body=%s)", body)
	}
	assertJSONKeys(t, "data", rawData, "checks", "status")
}

// assertCheckItemShape 断言单个检查项的键集合：
// 恒有 {name,status}；有 error 明细时恒有 {error}（error 是 omitempty）。
func assertCheckItemShape(t *testing.T, label string, raw []byte, hasError bool) {
	t.Helper()
	want := []string{"name", "status"}
	if hasError {
		want = append(want, "error")
	}
	assertJSONKeys(t, label, raw, want...)
}

// TestHealthResponse_ExportedSurfaceMatchesActualJSON 用导出的类型解码真实响应体，
// 断言逐项 name/status/error 全部正确（degraded 与 error 两种状态 + 多个 checker）。
//
// 这是「导出面 ↔ 实际 JSON」一致性的防回归测试：
// 任何 json tag 写错/字段缺失，这里都会变红（键集合对不上或解码值不匹配）。
func TestHealthResponse_ExportedSurfaceMatchesActualJSON(t *testing.T) {
	t.Run("混合 degraded 与 error（HTTP 503）", func(t *testing.T) {
		code, body := performHealth(t,
			health.WithCheckers(
				health.NamedChecker("db", func(ctx context.Context) error { return nil }),
				health.NamedChecker("cache", func(ctx context.Context) error {
					return health.Degraded(errors.New("cache unreachable"))
				}),
				health.NamedChecker("mcp-tools", func(ctx context.Context) error {
					return errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")
				}),
			),
			health.WithVerboseErrors(),
		)

		if code != consts.StatusServiceUnavailable {
			t.Fatalf("HTTP status = %d, want 503", code)
		}

		// 字面键契约：json tag 写错时这里立刻变红
		assertEnvelopeShape(t, body)

		resp := decodeHealthResponse(t, body)

		// 信封层
		if resp.Code != 503 {
			t.Errorf("code = %d, want 503", resp.Code)
		}
		if resp.Message != "service unhealthy" {
			t.Errorf("message = %q, want %q", resp.Message, "service unhealthy")
		}

		// 整体状态（data 层，不在顶层）
		if resp.Data.Status != health.StatusError {
			t.Errorf("data.status = %q, want %q", resp.Data.Status, health.StatusError)
		}

		// 逐项结果：name / status / error
		if len(resp.Data.Checks) != 3 {
			t.Fatalf("len(data.checks) = %d, want 3 (body=%s)", len(resp.Data.Checks), body)
		}
		want := []health.CheckResult{
			{Name: "db", Status: health.StatusOK, Error: ""},
			{Name: "cache", Status: health.StatusDegraded, Error: "cache unreachable"},
			{Name: "mcp-tools", Status: health.StatusError, Error: "dial tcp 127.0.0.1:9000: connect: connection refused"},
		}
		for i, w := range want {
			got := resp.Data.Checks[i]
			if got.Name != w.Name {
				t.Errorf("data.checks[%d].name = %q, want %q", i, got.Name, w.Name)
			}
			if got.Status != w.Status {
				t.Errorf("data.checks[%d].status = %q, want %q", i, got.Status, w.Status)
			}
			if got.Error != w.Error {
				t.Errorf("data.checks[%d].error = %q, want %q", i, got.Error, w.Error)
			}
		}

		// 逐项键名契约：ok 项无 error，degraded/error 项有 error
		var rawChecks []json.RawMessage
		if err := json.Unmarshal(mustDataField(t, body, "checks"), &rawChecks); err != nil {
			t.Fatalf("unmarshal data.checks: %v", err)
		}
		if len(rawChecks) != len(want) {
			t.Fatalf("len(raw data.checks) = %d, want %d", len(rawChecks), len(want))
		}
		for i, w := range want {
			assertCheckItemShape(t, "data.checks["+w.Name+"]", rawChecks[i], w.Error != "")
		}
	})

	t.Run("仅 degraded（HTTP 200）", func(t *testing.T) {
		code, body := performHealth(t,
			health.WithCheckers(
				health.NamedChecker("db", func(ctx context.Context) error { return nil }),
				health.NamedChecker("cache", func(ctx context.Context) error {
					return health.Degraded(errors.New("cache unreachable"))
				}),
			),
		)

		if code != consts.StatusOK {
			t.Fatalf("HTTP status = %d, want 200", code)
		}

		assertEnvelopeShape(t, body)

		resp := decodeHealthResponse(t, body)

		if resp.Code != 0 {
			t.Errorf("code = %d, want 0", resp.Code)
		}
		if resp.Message != "service degraded" {
			t.Errorf("message = %q, want %q", resp.Message, "service degraded")
		}
		if resp.Data.Status != health.StatusDegraded {
			t.Errorf("data.status = %q, want %q", resp.Data.Status, health.StatusDegraded)
		}
		if len(resp.Data.Checks) != 2 {
			t.Fatalf("len(data.checks) = %d, want 2 (body=%s)", len(resp.Data.Checks), body)
		}
		if resp.Data.Checks[1].Name != "cache" {
			t.Errorf("data.checks[1].name = %q, want %q", resp.Data.Checks[1].Name, "cache")
		}
		if resp.Data.Checks[1].Status != health.StatusDegraded {
			t.Errorf("data.checks[1].status = %q, want %q", resp.Data.Checks[1].Status, health.StatusDegraded)
		}
		// 默认脱敏：单项详情是 "check degraded"，不是原始错误文本
		if resp.Data.Checks[1].Error != "check degraded" {
			t.Errorf("data.checks[1].error = %q, want %q", resp.Data.Checks[1].Error, "check degraded")
		}
	})

	t.Run("全部通过（HTTP 200）", func(t *testing.T) {
		code, body := performHealth(t,
			health.WithCheckers(
				health.NamedChecker("db", func(ctx context.Context) error { return nil }),
				health.NamedChecker("redis", func(ctx context.Context) error { return nil }),
			),
		)

		if code != consts.StatusOK {
			t.Fatalf("HTTP status = %d, want 200", code)
		}

		assertEnvelopeShape(t, body)

		resp := decodeHealthResponse(t, body)

		if resp.Code != 0 {
			t.Errorf("code = %d, want 0", resp.Code)
		}
		if resp.Message != "success" {
			t.Errorf("message = %q, want %q", resp.Message, "success")
		}
		if resp.Data.Status != health.StatusOK {
			t.Errorf("data.status = %q, want %q", resp.Data.Status, health.StatusOK)
		}
		if len(resp.Data.Checks) != 2 {
			t.Fatalf("len(data.checks) = %d, want 2 (body=%s)", len(resp.Data.Checks), body)
		}
		for i, name := range []string{"db", "redis"} {
			if resp.Data.Checks[i].Name != name {
				t.Errorf("data.checks[%d].name = %q, want %q", i, resp.Data.Checks[i].Name, name)
			}
			if resp.Data.Checks[i].Status != health.StatusOK {
				t.Errorf("data.checks[%d].status = %q, want %q", i, resp.Data.Checks[i].Status, health.StatusOK)
			}
			if resp.Data.Checks[i].Error != "" {
				t.Errorf("data.checks[%d].error = %q, want empty", i, resp.Data.Checks[i].Error)
			}
		}
	})
}

// mustDataField 取 data 下的某个原始字段。
func mustDataField(t *testing.T, body []byte, field string) json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal top-level: %v (body=%s)", err, body)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &data); err != nil {
		t.Fatalf("unmarshal data: %v (body=%s)", err, body)
	}
	raw, ok := data[field]
	if !ok {
		t.Fatalf("data missing %q (body=%s)", field, body)
	}
	return raw
}

// TestHealthResponse_ResponseShapeIsDataNested 固化响应体的结构事实：
//
//  1. status / checks 都在 data 下，不在顶层
//  2. checks 是数组，不是 map
//  3. 单项详情在 error 字段（message 是顶层信封消息）
//  4. status 取值三态 ok / degraded / error
//
// 这四点正是消费方最容易猜错的地方，猜错的典型表现是「静默失效」：
// 字段对不上不报错，只是取不到值——健康状态与逐项失败项整个丢失。
func TestHealthResponse_ResponseShapeIsDataNested(t *testing.T) {
	_, body := performHealth(t,
		health.WithCheckers(
			health.NamedChecker("db", func(ctx context.Context) error { return nil }),
			health.NamedChecker("mcp-tools", func(ctx context.Context) error {
				return health.Degraded(errors.New("check degraded"))
			}),
		),
	)

	// 顶层只有 code / data / message / trace_id
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal top-level: %v (body=%s)", err, body)
	}
	for _, key := range []string{"code", "data", "message", "trace_id"} {
		if _, ok := top[key]; !ok {
			t.Errorf("top-level missing %q (body=%s)", key, body)
		}
	}
	if _, ok := top["status"]; ok {
		t.Errorf("top-level must NOT contain \"status\" (it lives under data), body=%s", body)
	}
	if _, ok := top["checks"]; ok {
		t.Errorf("top-level must NOT contain \"checks\" (it lives under data), body=%s", body)
	}

	// data 下才有 status / checks
	var data map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &data); err != nil {
		t.Fatalf("unmarshal data: %v (data=%s)", err, top["data"])
	}
	if _, ok := data["status"]; !ok {
		t.Errorf("data missing \"status\" (data=%s)", top["data"])
	}
	rawChecks, ok := data["checks"]
	if !ok {
		t.Fatalf("data missing \"checks\" (data=%s)", top["data"])
	}

	// checks 是数组，不是 map
	var arr []json.RawMessage
	if err := json.Unmarshal(rawChecks, &arr); err != nil {
		t.Fatalf("data.checks is not a JSON array: %v (checks=%s)", err, rawChecks)
	}
	if len(arr) != 2 {
		t.Errorf("len(data.checks) = %d, want 2 (checks=%s)", len(arr), rawChecks)
	}

	// 单项字段名：name / status / error（详情在 error，不是 message）
	var item map[string]json.RawMessage
	if err := json.Unmarshal(arr[1], &item); err != nil {
		t.Fatalf("unmarshal data.checks[1]: %v", err)
	}
	for _, key := range []string{"name", "status", "error"} {
		if _, ok := item[key]; !ok {
			t.Errorf("data.checks[1] missing %q (item=%s)", key, arr[1])
		}
	}
	if _, ok := item["message"]; ok {
		t.Errorf("data.checks[1] must NOT contain \"message\" (item detail lives in \"error\"), item=%s", arr[1])
	}

	// 状态三态：ok / degraded / error
	var status string
	if err := json.Unmarshal(data["status"], &status); err != nil {
		t.Fatalf("unmarshal data.status: %v", err)
	}
	switch status {
	case string(health.StatusOK), string(health.StatusDegraded), string(health.StatusError):
	default:
		t.Errorf("data.status = %q, want one of ok/degraded/error", status)
	}
}
