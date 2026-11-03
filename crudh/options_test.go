package crudh

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
	mw "github.com/oekit/middleware"
	bizerrors "github.com/oekit/pkg/errors"
)

// --- 可配置点测试：响应信封 / 错误码 / 路由布局（issue #94 诉求三件套）+ 默认行为逐字回归 ---

// itemHandler 是本文件测试统一使用的 Handler 实例化形态。
type itemHandler = Handler[Item, int64, CreateItemReq, UpdateItemReq]

// newEngineWith 挂载 handler 的路由并返回可发请求的引擎。
func newEngineWith(t *testing.T, h *itemHandler, prefix string) *route.Engine {
	t.Helper()
	engine := route.NewEngine(config.NewOptions(nil))
	h.RegisterRoutes(engine.Group("/api"), prefix)
	return engine
}

// doJSON 经 ut 发送 JSON 请求（不走网络）。
func doJSON(t *testing.T, engine *route.Engine, method, path, body string) *ut.ResponseRecorder {
	t.Helper()
	var b *ut.Body
	if body != "" {
		b = &ut.Body{Body: bytes.NewReader([]byte(body)), Len: len(body)}
	}
	return ut.PerformRequest(engine, method, path, b, ut.Header{Key: "Content-Type", Value: "application/json"})
}

// decodeEnvelope 解析统一信封 {code,data,message,trace_id}。
func decodeEnvelope(t *testing.T, w *ut.ResponseRecorder) apiResponse {
	t.Helper()
	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response body %q: %v", w.Body.Bytes(), err)
	}
	return resp
}

// --- 信封 DTO（zeus 形态样例） ---

type idResp struct {
	ID int64 `json:"id"`
}

type listResp struct {
	List []Item `json:"list"`
}

type emptyResp struct {
	Success bool `json:"success"`
}

// zeusWriter 是消费方信封 DTO 映射钩子样例：
// Create/Update→IDResp{ID}、List→ListXxxResp{List}、Delete→EmptyResp{Success}，
// Get 透传实体；信封沿用 mw.SuccessResp 同形 {code,data,message,trace_id}。
func zeusWriter(ops *[]Op) ResponseWriter {
	return func(ctx context.Context, c *app.RequestContext, op Op, data any) {
		*ops = append(*ops, op)
		switch op {
		case OpCreate, OpUpdate:
			mw.SuccessResp(c, idResp{ID: data.(*Item).ID})
		case OpGet:
			mw.SuccessResp(c, data)
		case OpList:
			mw.SuccessResp(c, listResp{List: data.(*crud.PageResponse[Item]).Items})
		case OpDelete:
			mw.SuccessResp(c, emptyResp{Success: true})
		}
	}
}

// TestWithResponseWriter_CustomEnvelope 三类可配置点①：entity/PageResponse 经
// 钩子映射为消费方信封 DTO（IDResp / ListXxxResp / EmptyResp），Op 判别正确。
func TestWithResponseWriter_CustomEnvelope(t *testing.T) {
	var ops []Op
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithResponseWriter[Item, int64, CreateItemReq, UpdateItemReq](zeusWriter(&ops)))
	engine := newEngineWith(t, h, "/items")

	// Create → IDResp{ID}（不再是裸 entity）
	w := doJSON(t, engine, consts.MethodPost, "/api/items", `{"name":"widget"}`)
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 0, resp.Code)
	assert.DeepEqual(t, `{"id":1}`, string(resp.Data))

	// Get → 透传实体
	w = doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, `{"id":1,"name":"widget"}`, string(resp.Data))

	// List → ListXxxResp{List}
	w = doJSON(t, engine, consts.MethodGet, "/api/items", "")
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, `{"list":[{"id":1,"name":"widget"}]}`, string(resp.Data))

	// Update → IDResp{ID}
	w = doJSON(t, engine, consts.MethodPut, "/api/items/1", `{"name":"new"}`)
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, `{"id":1}`, string(resp.Data))

	// Delete → EmptyResp{Success}
	w = doJSON(t, engine, consts.MethodDelete, "/api/items/1", "")
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, `{"success":true}`, string(resp.Data))

	assert.DeepEqual(t, []Op{OpCreate, OpGet, OpList, OpUpdate, OpDelete}, ops)
}

// TestDefaultSuccessEnvelope_ByteExact 默认行为逐字回归①：未注入 ResponseWriter 时
// 成功响应与历史 mw.SuccessResp 输出逐字一致（Get data=entity、Delete data=null）。
func TestDefaultSuccessEnvelope_ByteExact(t *testing.T) {
	h, svc := newHandler()
	engine := newEngineWith(t, h, "/items")

	if _, err := svc.Create(context.Background(), &CreateItemReq{Name: "found"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	w := doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
	assert.DeepEqual(t, `{"code":0,"data":{"id":1,"name":"found"},"message":"success","trace_id":""}`, w.Body.String())

	w = doJSON(t, engine, consts.MethodDelete, "/api/items/1", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
	assert.DeepEqual(t, `{"code":0,"data":null,"message":"success","trace_id":""}`, w.Body.String())
}

// TestDefaultErrorEnvelope_ByteExact 默认行为逐字回归②：未注入 ErrMapper 时
// 本地参数错误与历史 mw.ErrorResp(c, 400, "invalid id") 输出逐字一致
// （经 mw.Fail 渲染 BizError(400) 与 ErrorResp 同形）。
func TestDefaultErrorEnvelope_ByteExact(t *testing.T) {
	h, _ := newHandler()
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/abc", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, `{"code":400,"data":null,"message":"invalid id","trace_id":""}`, w.Body.String())

	w = doJSON(t, engine, consts.MethodGet, "/api/items?page=x", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, `{"code":400,"data":null,"message":"invalid page parameter","trace_id":""}`, w.Body.String())
}

// zeusErrMapper 是消费方码表映射样例：404→40401、400→40001、其余→50001。
func zeusErrMapper(err error) error {
	var be *bizerrors.BizError
	if stderrors.As(err, &be) {
		switch be.BizCode {
		case 404:
			return bizerrors.NewBizError(40401, "资源不存在")
		case 400:
			return bizerrors.NewBizError(40001, "参数错误")
		}
	}
	return bizerrors.NewBizError(50001, "系统错误")
}

// TestWithErrMapper_CustomCodes 三类可配置点②：crud 错误（含 not-found、
// 路径参数解析、服务层错误）经钩子映射为自有码表，渲染仍走 mw.Fail 信封。
func TestWithErrMapper_CustomCodes(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithErrMapper[Item, int64, CreateItemReq, UpdateItemReq](zeusErrMapper))
	engine := newEngineWith(t, h, "/items")

	// not-found：404 → 40401（issue 里 mw.Fail 信封 body.code=40401 的契约）
	w := doJSON(t, engine, consts.MethodGet, "/api/items/999", "")
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 40401, resp.Code)
	assert.DeepEqual(t, "资源不存在", resp.Message)

	// 路径参数解析错误同样进映射：400 → 40001
	w = doJSON(t, engine, consts.MethodGet, "/api/items/abc", "")
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, 40001, resp.Code)

	// 服务层错误：500 → 50001
	svc.createErr = bizerrors.NewBizError(500, "db down")
	w = doJSON(t, engine, consts.MethodPost, "/api/items", `{"name":"x"}`)
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, 50001, resp.Code)

	// 绑定校验错误同样走钩子（kitx validator 包装为 BizError(400) 形态 → 40001）
	svc.createErr = nil
	w = doJSON(t, engine, consts.MethodPost, "/api/items", `{"name":""}`)
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, 40001, resp.Code)

	// 非 BizError 服务错误 → 兜底映射 50001
	svc.createErr = stderrors.New("boom")
	w = doJSON(t, engine, consts.MethodPost, "/api/items", `{"name":"x"}`)
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, 50001, resp.Code)
}

// TestWithErrMapper_NilReturnKeepsOriginal ErrMapper 返回 nil 时保留原错误（404 不变）。
func TestWithErrMapper_NilReturnKeepsOriginal(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithErrMapper[Item, int64, CreateItemReq, UpdateItemReq](func(error) error { return nil }))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/999", "")
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 404, resp.Code)
}

// refreshHandler 是动作端点（/cache/refresh）样例。
func refreshHandler(_ context.Context, c *app.RequestContext) {
	mw.SuccessResp(c, map[string]bool{"refreshed": true})
}

// TestWithRouteTemplate_CustomLayout 三类可配置点③：路由布局定制——
// 子资源（/:type/items）与动作端点（/cache/refresh）同标准 CRUD 并存，
// 且可省略端点（无 GET :id，与 zeus params 形态一致）。
func TestWithRouteTemplate_CustomLayout(t *testing.T) {
	var ops []Op
	recorder := func(ctx context.Context, c *app.RequestContext, op Op, data any) {
		ops = append(ops, op)
		mw.SuccessResp(c, data)
	}
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithResponseWriter[Item, int64, CreateItemReq, UpdateItemReq](recorder),
		WithRouteTemplate[Item, int64, CreateItemReq, UpdateItemReq](
			Route{Method: consts.MethodPost, Path: "", Op: OpCreate},
			Route{Method: consts.MethodGet, Path: "", Op: OpList},
			Route{Method: consts.MethodGet, Path: "/cache/refresh", Handler: refreshHandler},
			Route{Method: consts.MethodGet, Path: "/:type/items", Op: OpList},
			Route{Method: consts.MethodPut, Path: "/:id", Op: OpUpdate},
			Route{Method: consts.MethodDelete, Path: "/:id", Op: OpDelete},
		))
	engine := newEngineWith(t, h, "/dicts")

	// 动作端点可达
	w := doJSON(t, engine, consts.MethodGet, "/api/dicts/cache/refresh", "")
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 0, resp.Code)
	assert.DeepEqual(t, `{"refreshed":true}`, string(resp.Data))

	// 子资源列表可达（路径参数 :type）
	w = doJSON(t, engine, consts.MethodGet, "/api/dicts/general/items", "")
	resp = decodeEnvelope(t, w)
	assert.DeepEqual(t, 0, resp.Code)
	assert.DeepEqual(t, true, len(resp.Data) > 0)

	// 标准 CRUD 端点可达
	w = doJSON(t, engine, consts.MethodPost, "/api/dicts", `{"name":"x"}`)
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)
	w = doJSON(t, engine, consts.MethodGet, "/api/dicts", "")
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)
	w = doJSON(t, engine, consts.MethodPut, "/api/dicts/1", `{"name":"y"}`)
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)
	w = doJSON(t, engine, consts.MethodDelete, "/api/dicts/1", "")
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)

	// 省略 GET :id —— 该端点不注册（zeus params 形态：无 GET :id）：
	// 请求不得到达 crudh 的 Get。hertz 路由器对「同层仅通配子树」的未匹配 GET
	// 会给尾斜杠重定向（301→404），故此处只约束契约面：不绑定、不服务。
	before := len(ops)
	w = doJSON(t, engine, consts.MethodGet, "/api/dicts/1", "")
	assert.DeepEqual(t, before, len(ops))
	if w.Code == consts.StatusOK {
		t.Errorf("GET /api/dicts/1 must not be served by crudh, got %d", w.Code)
	}
}

// TestWithRouteTemplate_EmptyRegistersNothing 空模板 = 不注册任何路由。
func TestWithRouteTemplate_EmptyRegistersNothing(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithRouteTemplate[Item, int64, CreateItemReq, UpdateItemReq]())
	engine := newEngineWith(t, h, "/items")

	for _, path := range []string{"/api/items", "/api/items/1"} {
		w := doJSON(t, engine, consts.MethodGet, path, "")
		assert.DeepEqual(t, consts.StatusNotFound, w.Code)
	}
}

// TestDefaultRoutes_Layout 默认行为逐字回归③：缺省路由布局不变——
// POST/GET prefix、GET/PUT/DELETE prefix/:id 全部端到端可达。
func TestDefaultRoutes_Layout(t *testing.T) {
	h, _ := newHandler()
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodPost, "/api/items", `{"name":"a"}`)
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)

	w = doJSON(t, engine, consts.MethodGet, "/api/items", "")
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)

	w = doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 0, resp.Code)
	var got Item
	if err := json.Unmarshal(resp.Data, &got); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	assert.DeepEqual(t, "a", got.Name)

	w = doJSON(t, engine, consts.MethodPut, "/api/items/1", `{"name":"b"}`)
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)

	w = doJSON(t, engine, consts.MethodDelete, "/api/items/1", "")
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)

	w = doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	assert.DeepEqual(t, 404, decodeEnvelope(t, w).Code)
}

// TestDefaultRoutes_FreshCopy DefaultRoutes 每次返回新副本，且与缺省布局逐条一致。
func TestDefaultRoutes_FreshCopy(t *testing.T) {
	r1 := DefaultRoutes()
	assert.DeepEqual(t, 5, len(r1))
	assert.DeepEqual(t, Route{Method: "POST", Path: "", Op: OpCreate}, r1[0])
	assert.DeepEqual(t, Route{Method: "GET", Path: "", Op: OpList}, r1[1])
	assert.DeepEqual(t, Route{Method: "GET", Path: "/:id", Op: OpGet}, r1[2])
	assert.DeepEqual(t, Route{Method: "PUT", Path: "/:id", Op: OpUpdate}, r1[3])
	assert.DeepEqual(t, Route{Method: "DELETE", Path: "/:id", Op: OpDelete}, r1[4])

	r1[0] = Route{Method: "PATCH", Path: "/x", Op: OpGet}
	r2 := DefaultRoutes()
	assert.DeepEqual(t, Route{Method: "POST", Path: "", Op: OpCreate}, r2[0])
}

// TestWithRouteTemplate_InvalidPanics 非法模板在构造期 panic（fail-fast）。
func TestWithRouteTemplate_InvalidPanics(t *testing.T) {
	cases := map[string]Route{
		"empty method":        {Path: "", Op: OpCreate},
		"bad path":            {Method: "GET", Path: "no-slash", Op: OpCreate},
		"op and handler both": {Method: "GET", Path: "/x", Op: OpCreate, Handler: refreshHandler},
		"neither":             {Method: "GET", Path: "/x"},
		"unknown op":          {Method: "GET", Path: "/x", Op: "bogus"},
	}
	for name, r := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			WithRouteTemplate[Item, int64, CreateItemReq, UpdateItemReq](r)
		}()
	}
}
