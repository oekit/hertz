package crudh

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/oekit/gormx/crud"
	bizerrors "github.com/oekit/pkg/errors"
)

// --- issue #101 三处 Op 语义缺口的自测（判据逐条取自 issue 正文） ---
//
// 缺口① List 查询参数绑定写死：可关 / 可自定义 QueryBinder。
// 缺口② id 解析失败出口写死：可定制（WithIDParseErrorHandler / 解析器错误即出口）。
// 缺口③ Delete found 策略写死：missing→success / →not-found 可配 + 级联钩子。

// spyItemService 记录 List 收到的查询、List/Delete 调用次数、Delete 收到的 ctx
// 与事件顺序：自测据此断言「绑定是否真的生效」「删除是否真的发生」
// 「级联钩子是否在删除之前执行」「删除执行器是否用调用方 ctx」。
type spyItemService struct {
	*mockItemService

	spyMu       sync.Mutex
	listCalls   int
	lastQuery   *crud.Query
	deleteCalls int
	deleteCtx   context.Context
	events      []string
}

func newSpyItemService() *spyItemService {
	return &spyItemService{mockItemService: newMockItemService()}
}

func (s *spyItemService) List(ctx context.Context, q *crud.Query) (*crud.PageResponse[Item], error) {
	s.spyMu.Lock()
	s.listCalls++
	s.lastQuery = q
	s.spyMu.Unlock()
	return s.mockItemService.List(ctx, q)
}

func (s *spyItemService) Delete(ctx context.Context, id int64) error {
	s.spyMu.Lock()
	s.deleteCalls++
	s.deleteCtx = ctx
	s.events = append(s.events, "delete")
	s.spyMu.Unlock()
	return s.mockItemService.Delete(ctx, id)
}

// record 记录级联钩子等外部事件（与 Delete 的事件共用一条时间线）。
func (s *spyItemService) record(ev string) {
	s.spyMu.Lock()
	s.events = append(s.events, ev)
	s.spyMu.Unlock()
}

func (s *spyItemService) snapshot() (listCalls int, deleteCalls int, q *crud.Query, events []string) {
	s.spyMu.Lock()
	defer s.spyMu.Unlock()
	return s.listCalls, s.deleteCalls, s.lastQuery, append([]string(nil), s.events...)
}

// --- 缺口①：List 查询参数绑定可关 / 可自定义 ---

// TestListParamsDisabled_IgnoresQueryParams 缺口①判据：查询参数绑定可关——
// WithListParams(false) 后非法分页/排序参数不再 400，且查询不带分页与排序
// （存量 IDL BindAndValidate 契约：忽略这些参数、恒全量返回）。
func TestListParamsDisabled_IgnoresQueryParams(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithListParams[Item, int64, CreateItemReq, UpdateItemReq](false))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?page=x&page_size=y&sort="+"(SELECT 1)", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
	assert.DeepEqual(t, 0, decodeEnvelope(t, w).Code)

	listCalls, _, q, _ := svc.snapshot()
	assert.DeepEqual(t, 1, listCalls)
	assert.NotNil(t, q)
	assert.Nil(t, q.GetPage())
	assert.DeepEqual(t, 0, len(q.Spec().Orders))
	assert.DeepEqual(t, 0, len(q.Spec().Conds))
}

// TestListDefaultBinding_Unchanged 缺口①判据（缺省兼容）：未配置时仍绑定
// page/page_size/sort，非法分页值仍逐字 400。
func TestListDefaultBinding_Unchanged(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc)
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?page=2&page_size=5&sort=name", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)

	_, _, q, _ := svc.snapshot()
	assert.NotNil(t, q.GetPage())
	assert.DeepEqual(t, 2, q.GetPage().Page)
	assert.DeepEqual(t, 5, q.GetPage().PageSize)
	assert.DeepEqual(t, []string{"name"}, q.Spec().Orders)

	// 非法分页值仍 400（逐字回归）
	w = doJSON(t, engine, consts.MethodGet, "/api/items?page=x", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, `{"code":400,"data":null,"message":"invalid page parameter","trace_id":""}`, w.Body.String())
}

// TestWithQueryBinder_ReplacesDefaultBinding 缺口①判据：QueryBinder 可替换
// 缺省绑定——自定义钩子全权接管（缺省分页/排序不再执行），自定义条件生效。
func TestWithQueryBinder_ReplacesDefaultBinding(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithQueryBinder[Item, int64, CreateItemReq, UpdateItemReq](
			func(_ context.Context, c *app.RequestContext, q *crud.Query) error {
				if kw := c.Query("keyword"); kw != "" {
					q.Where("name", "LIKE", kw)
				}
				return nil
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?page=2&page_size=5&sort=name&keyword=abc", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)

	_, _, q, _ := svc.snapshot()
	assert.NotNil(t, q)
	assert.Nil(t, q.GetPage())
	assert.DeepEqual(t, 0, len(q.Spec().Orders))
	conds := q.Spec().Conds
	assert.DeepEqual(t, 1, len(conds))
	assert.DeepEqual(t, "name LIKE ?", conds[0].Query)
	assert.DeepEqual(t, []any{"abc"}, conds[0].Args)
}

// TestWithQueryBinder_ComposesWithDefaultBinding 缺口①判据：自定义钩子可复用
// 缺省绑定口径（BindDefaultQuery）——保留缺省分页排序并叠加自定义过滤。
func TestWithQueryBinder_ComposesWithDefaultBinding(t *testing.T) {
	svc := newSpyItemService()
	var h *itemHandler
	h = NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithQueryBinder[Item, int64, CreateItemReq, UpdateItemReq](
			func(_ context.Context, c *app.RequestContext, q *crud.Query) error {
				if bindErr := h.BindDefaultQuery(c, q); bindErr != nil {
					return bindErr
				}
				if kw := c.Query("keyword"); kw != "" {
					q.Where("name", "LIKE", kw)
				}
				return nil
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?page=3&page_size=7&keyword=abc", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)

	_, _, q, _ := svc.snapshot()
	assert.NotNil(t, q.GetPage())
	assert.DeepEqual(t, 3, q.GetPage().Page)
	assert.DeepEqual(t, 7, q.GetPage().PageSize)
	assert.DeepEqual(t, 1, len(q.Spec().Conds))
	assert.DeepEqual(t, "name LIKE ?", q.Spec().Conds[0].Query)
}

// TestWithQueryBinder_ErrorAbortsQuery 缺口①判据：自定义绑定失败时按钩子给的
// 错误出口（自有码表），且不发起查询。
func TestWithQueryBinder_ErrorAbortsQuery(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithQueryBinder[Item, int64, CreateItemReq, UpdateItemReq](
			func(context.Context, *app.RequestContext, *crud.Query) error {
				return bizerrors.NewBizError(40001, "参数错误")
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?keyword=abc", "")
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 40001, resp.Code)
	assert.DeepEqual(t, "参数错误", resp.Message)

	listCalls, _, _, _ := svc.snapshot()
	assert.DeepEqual(t, 0, listCalls)
}

// --- 缺口②：id 解析失败出口可定制 ---

// TestIDParseErrorHandler_CustomExit 缺口②判据：id 解析失败出口可定制——
// Get/Update/Delete 三条路径都走钩子给出的自有错误（自有码 40001）。
func TestIDParseErrorHandler_CustomExit(t *testing.T) {
	var gotErr error
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithIDParseErrorHandler[Item, int64, CreateItemReq, UpdateItemReq](
			func(_ context.Context, _ *app.RequestContext, err error) error {
				gotErr = err
				return bizerrors.NewBizError(40001, "ID 非法")
			}))
	engine := newEngineWith(t, h, "/items")

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{consts.MethodGet, "/api/items/abc", ""},
		{consts.MethodPut, "/api/items/abc", `{"name":"x"}`},
		{consts.MethodDelete, "/api/items/abc", ""},
	}
	for _, tc := range cases {
		gotErr = nil
		w := doJSON(t, engine, tc.method, tc.path, tc.body)
		resp := decodeEnvelope(t, w)
		assert.DeepEqual(t, 40001, resp.Code)
		assert.DeepEqual(t, "ID 非法", resp.Message)
		if gotErr == nil {
			t.Fatalf("%s %s: id parse error must be passed to the exit hook", tc.method, tc.path)
		}
	}

	_, deleteCalls, _, _ := svc.snapshot()
	assert.DeepEqual(t, 0, deleteCalls)
}

// TestIDParseErrorPassthrough_ParserErrorIsExit 缺口②判据：IDParser 自带错误
// 即出口（映射交 WithErrMapper）——解析器返回的自有码 40001 原样出参。
func TestIDParseErrorPassthrough_ParserErrorIsExit(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithIDParser[Item, int64, CreateItemReq, UpdateItemReq](
			func(string) (int64, error) {
				return 0, bizerrors.NewBizError(40001, "ID 格式错")
			}),
		WithIDParseErrorHandler[Item, int64, CreateItemReq, UpdateItemReq](IDParseErrorPassthrough),
		WithErrMapper[Item, int64, CreateItemReq, UpdateItemReq](func(err error) error {
			var be *bizerrors.BizError
			if stderrors.As(err, &be) && be.BizCode == 40001 {
				return bizerrors.NewBizError(40001, "参数错误")
			}
			return nil
		}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/abc", "")
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 40001, resp.Code)
	assert.DeepEqual(t, "参数错误", resp.Message)
}

// TestIDParseErrorHandler_NilFallsBackToDefault 缺口②判据：钩子返回 nil 时
// 回退缺省出口（不吞错误），输出与历史逐字一致。
func TestIDParseErrorHandler_NilFallsBackToDefault(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithIDParseErrorHandler[Item, int64, CreateItemReq, UpdateItemReq](
			func(context.Context, *app.RequestContext, error) error { return nil }))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/abc", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, `{"code":400,"data":null,"message":"invalid id","trace_id":""}`, w.Body.String())
}

// TestIDParseErrorDefault_ByteExact 缺口②判据（缺省兼容）：未注入钩子时，
// Get/Update/Delete 的解析失败输出与历史逐字一致（400 + "invalid id"）。
func TestIDParseErrorDefault_ByteExact(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc)
	engine := newEngineWith(t, h, "/items")

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{consts.MethodGet, "/api/items/abc", ""},
		{consts.MethodPut, "/api/items/abc", `{"name":"x"}`},
		{consts.MethodDelete, "/api/items/abc", ""},
	}
	for _, tc := range cases {
		w := doJSON(t, engine, tc.method, tc.path, tc.body)
		assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
		assert.DeepEqual(t, `{"code":400,"data":null,"message":"invalid id","trace_id":""}`, w.Body.String())
	}
}

// --- 缺口③：Delete found 策略 + 级联钩子 ---

// TestDeleteFoundPolicy_DefaultKeepsNotFound 缺口③判据（缺省兼容）：未配置时
// 记录不存在仍按 not-found 出参。
func TestDeleteFoundPolicy_DefaultKeepsNotFound(t *testing.T) {
	svc := newSpyItemService()
	svc.deleteErr = bizerrors.NewBizError(404, "not found")
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc)
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodDelete, "/api/items/999", "")
	assert.DeepEqual(t, consts.StatusNotFound, w.Code)
	assert.DeepEqual(t, 404, decodeEnvelope(t, w).Code)
}

// TestDeleteFoundPolicy_SuccessIsIdempotent 缺口③判据：found 策略 missing→success
// ——删除不存在的记录按成功出参（存量 DeleteDictItem「删不存在 = 200」契约）。
// 404 业务码与 crud.ErrNotFound 哨兵两种形态都命中。
func TestDeleteFoundPolicy_SuccessIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"biz code 404", bizerrors.NewBizError(404, "not found")},
		{"sentinel", crud.ErrNotFound},
	} {
		svc := newSpyItemService()
		svc.deleteErr = tc.err
		h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
			WithDeleteFoundPolicy[Item, int64, CreateItemReq, UpdateItemReq](DeleteFoundSuccess))
		engine := newEngineWith(t, h, "/items")

		w := doJSON(t, engine, consts.MethodDelete, "/api/items/999", "")
		assert.DeepEqual(t, consts.StatusOK, w.Code)
		assert.DeepEqual(t, `{"code":0,"data":null,"message":"success","trace_id":""}`, w.Body.String())
	}
}

// TestDeleteFoundPolicy_SuccessKeepsNonNotFoundErrors 缺口③判据：幂等口径只作用于
// 「记录不存在」，其他错误（如 500）照常失败，不得被吞成成功。
func TestDeleteFoundPolicy_SuccessKeepsNonNotFoundErrors(t *testing.T) {
	svc := newSpyItemService()
	svc.deleteErr = bizerrors.NewBizError(500, "db down")
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithDeleteFoundPolicy[Item, int64, CreateItemReq, UpdateItemReq](DeleteFoundSuccess))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodDelete, "/api/items/1", "")
	assert.DeepEqual(t, consts.StatusInternalServerError, w.Code)
	assert.DeepEqual(t, 500, decodeEnvelope(t, w).Code)
}

// TestDeleteCascade_HookRunsBeforeDelete 缺口③判据：级联钩子在主记录删除之前
// 执行（前置级联语义），钩子拿到解析后的主键。
func TestDeleteCascade_HookRunsBeforeDelete(t *testing.T) {
	svc := newSpyItemService()
	var gotID int64
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithDeleteCascade[Item, int64, CreateItemReq, UpdateItemReq](
			func(_ context.Context, _ *app.RequestContext, id int64, _ func(context.Context) error) error {
				gotID = id
				svc.record("cascade")
				return nil
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodDelete, "/api/items/7", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
	assert.DeepEqual(t, int64(7), gotID)

	_, deleteCalls, _, events := svc.snapshot()
	assert.DeepEqual(t, 1, deleteCalls)
	assert.DeepEqual(t, []string{"cascade", "delete"}, events)
}

// TestDeleteCascade_HookErrorAbortsDelete 缺口③判据：级联钩子失败即中止删除
// （主记录不删），错误经统一出口出参。
func TestDeleteCascade_HookErrorAbortsDelete(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithDeleteCascade[Item, int64, CreateItemReq, UpdateItemReq](
			func(_ context.Context, _ *app.RequestContext, _ int64, _ func(context.Context) error) error {
				svc.record("cascade")
				return bizerrors.NewBizError(409, "级联失败")
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodDelete, "/api/items/7", "")
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, 409, resp.Code)
	assert.DeepEqual(t, "级联失败", resp.Message)

	_, deleteCalls, _, events := svc.snapshot()
	assert.DeepEqual(t, 0, deleteCalls)
	assert.DeepEqual(t, []string{"cascade"}, events)
}

// TestDeleteCascade_HookOwnsDeleteViaExecutor 缺口③判据：钩子可调用删除执行器
// 接管主记录删除（钩子返回 nil 且已调用 del 时，handler 不再重复删除）。
func TestDeleteCascade_HookOwnsDeleteViaExecutor(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithDeleteCascade[Item, int64, CreateItemReq, UpdateItemReq](
			func(ctx context.Context, _ *app.RequestContext, _ int64, del func(context.Context) error) error {
				svc.record("cascade")
				return del(ctx)
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodDelete, "/api/items/7", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)

	_, deleteCalls, _, events := svc.snapshot()
	assert.DeepEqual(t, 1, deleteCalls)
	assert.DeepEqual(t, []string{"cascade", "delete"}, events)
}

// TestDeleteCascade_ExecutorUsesCallerContext 缺口③判据：删除执行器使用调用方
// 传入的 ctx（消费方可在钩子内开事务，把事务 ctx 传给 del 实现同事务级联）。
func TestDeleteCascade_ExecutorUsesCallerContext(t *testing.T) {
	type ctxKey struct{}
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithDeleteCascade[Item, int64, CreateItemReq, UpdateItemReq](
			func(_ context.Context, _ *app.RequestContext, _ int64, del func(context.Context) error) error {
				txCtx := context.WithValue(context.Background(), ctxKey{}, "tx")
				return del(txCtx)
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodDelete, "/api/items/7", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)

	svc.spyMu.Lock()
	defer svc.spyMu.Unlock()
	if svc.deleteCtx == nil {
		t.Fatal("delete was not executed")
	}
	if got := svc.deleteCtx.Value(ctxKey{}); got != "tx" {
		t.Fatalf("delete ctx value = %v, want \"tx\"", got)
	}
}

// TestDeleteCascade_NotFoundUnderSuccessPolicy 缺口③判据：级联钩子返回的
// 「记录不存在」错误同样受 found 策略约束（幂等口径下按成功出参）。
func TestDeleteCascade_NotFoundUnderSuccessPolicy(t *testing.T) {
	svc := newSpyItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithDeleteFoundPolicy[Item, int64, CreateItemReq, UpdateItemReq](DeleteFoundSuccess),
		WithDeleteCascade[Item, int64, CreateItemReq, UpdateItemReq](
			func(_ context.Context, _ *app.RequestContext, _ int64, _ func(context.Context) error) error {
				return crud.ErrNotFound
			}))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodDelete, "/api/items/999", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
	assert.DeepEqual(t, `{"code":0,"data":null,"message":"success","trace_id":""}`, w.Body.String())
}
