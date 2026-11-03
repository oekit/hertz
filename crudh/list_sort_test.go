package crudh

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// --- v0.9.0 改造②：排序列白名单（缺省按实体字段反射生成）---
//
// v0.8.14 缺省：未设 WithSortableColumns ⇒ 白名单 nil ⇒ 只做标识符正则 ⇒
// 任意标识符列名都进 ORDER BY（列名原文拼进 SQL）⇒ 列不存在时 DB 报
// no such column ⇒ 客户端拿到 5xx 而不是 400。
//
// v0.9.0 缺省：白名单按实体类型 T 的字段反射生成 ⇒ 白名单外的列 → 400
// "invalid sort parameter"（逐字沿用历史文案）。
// 保留旧行为：WithSortValidation(false) ⇒ 全放开（只做标识符正则）。

// sortEntity 覆盖白名单反射的各类列名来源。
type sortEntity struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Secret    string    `json:"secret" gorm:"column:secret_col"`
	Hidden    string    `json:"-"`
	internal  string
}

// TestEntityColumns_ReflectsFieldsAndTags 白名单反射：候选列名 = 字段名（小写）
// + json tag 名（小写）+ gorm column tag 名（小写）。
//   - 字段名恒为候选（即使 json:"-"——字段仍是 DB 列，只是 API 不暴露）；
//   - json:"-" 只抑制 json tag 候选（不产生 "-" 候选名）；
//   - 非导出字段不进白名单（不是 DB 列）。
func TestEntityColumns_ReflectsFieldsAndTags(t *testing.T) {
	cols := entityColumns[sortEntity]()

	for _, want := range []string{"id", "name", "created_at", "createdat", "secret", "secret_col"} {
		if _, ok := cols[want]; !ok {
			t.Errorf("column %q missing in reflected set %v", want, cols)
		}
	}
	// json:"-" 的字段名仍是候选，但不得出现 "-" 这种非法列名
	if _, ok := cols["-"]; ok {
		t.Errorf("json:\"-\" must not produce a \"-\" column candidate: %v", cols)
	}
	// 非导出字段不进白名单
	ent := sortEntity{internal: "x"}
	if ent.internal != "x" {
		t.Fatal("unreachable")
	}
	if _, ok := cols["internal"]; ok {
		t.Errorf("unexported field must not be in reflected set %v", cols)
	}
}

// TestEntityColumns_NonStructEntity 空集：实体不是结构体 ⇒ 白名单为空 ⇒ 全拒绝。
func TestEntityColumns_NonStructEntity(t *testing.T) {
	cols := entityColumns[int]()
	assert.DeepEqual(t, 0, len(cols))
}

// TestList_SortUnknownColumn_400 白名单外的列 → 400（不再 5xx）。
// 文案逐字沿用历史对外契约 "invalid sort parameter"。
func TestList_SortUnknownColumn_400(t *testing.T) {
	h, _ := newHandler()
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?sort=nosuchcolumn", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, "{\"code\":400,\"data\":null,\"message\":\"invalid sort parameter\",\"trace_id\":\"\"}", w.Body.String())
}

// TestList_SortEntityColumns_Allowed 实体字段（字段名 / json tag 名）可排序。
func TestList_SortEntityColumns_Allowed(t *testing.T) {
	h, _ := newHandler()
	engine := newEngineWith(t, h, "/items")

	for _, s := range []string{"id", "name", "ID", "NAME", "id desc", "name asc"} {
		w := doJSON(t, engine, consts.MethodGet, "/api/items?sort="+s, "")
		if w.Code != consts.StatusOK {
			t.Errorf("sort=%q: expected 200, got %d body=%s", s, w.Code, w.Body.String())
		}
	}
}

// TestList_SortValidationDisabled_AllowsAnyIdentifier WithSortValidation(false)
// 时任意标识符列放行。
func TestList_SortValidationDisabled_AllowsAnyIdentifier(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithSortValidation[Item, int64, CreateItemReq, UpdateItemReq](false))
	engine := newEngineWith(t, h, "/items")

	for _, s := range []string{"nosuchcolumn", "password", "id desc"} {
		w := doJSON(t, engine, consts.MethodGet, "/api/items?sort="+s, "")
		if w.Code != consts.StatusOK {
			t.Errorf("sort=%q: expected 200 (validation off), got %d", s, w.Code)
		}
	}
}

// TestList_SortableColumnsExplicit_OverridesReflected 显式白名单以调用方为准
// （不再叠加反射结果）：只放行 name，id 也被拒。
func TestList_SortableColumnsExplicit_OverridesReflected(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithSortableColumns[Item, int64, CreateItemReq, UpdateItemReq]("name"))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?sort=name", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)

	w = doJSON(t, engine, consts.MethodGet, "/api/items?sort=id", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
}

// TestList_SortableColumnsEmpty_RejectsAll 显式空调用 = 空白名单 = 全拒绝
// （与「未设置」语义相反，两者不再混同）。
func TestList_SortableColumnsEmpty_RejectsAll(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithSortableColumns[Item, int64, CreateItemReq, UpdateItemReq]())
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?sort=id", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
}

// --- v0.9.0 改造③：路径参数名可配置 / 可推导 ---

// TestGet_IDParamDerivedFromRoute 路由模板用非 "id" 参数名（/:code）时，
// 路径参数名按模板推导 ⇒ 端点可用（v0.8.14 只读 c.Param("id") ⇒ 永远取不到值）。
func TestGet_IDParamDerivedFromRoute(t *testing.T) {
	svc := newMockItemService()
	if _, err := svc.Create(context.Background(), &CreateItemReq{Name: "found"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithRouteTemplate[Item, int64, CreateItemReq, UpdateItemReq](
			Route{Method: consts.MethodGet, Path: "", Op: OpList},
			Route{Method: consts.MethodGet, Path: "/:code", Op: OpGet},
			Route{Method: consts.MethodPut, Path: "/:code", Op: OpUpdate},
			Route{Method: consts.MethodDelete, Path: "/:code", Op: OpDelete},
		))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
	resp := decodeEnvelope(t, w)
	assert.DeepEqual(t, "{\"id\":1,\"name\":\"found\"}", string(resp.Data))
}

// TestGet_IDParamExplicit_WithIDParam 显式指定参数名优先于推导。
func TestGet_IDParamExplicit_WithIDParam(t *testing.T) {
	svc := newMockItemService()
	if _, err := svc.Create(context.Background(), &CreateItemReq{Name: "found"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithIDParam[Item, int64, CreateItemReq, UpdateItemReq]("code"),
		WithRouteTemplate[Item, int64, CreateItemReq, UpdateItemReq](
			Route{Method: consts.MethodGet, Path: "/:code", Op: OpGet},
		))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
}

// TestGet_IDParamExplicit_OverridesDerived 显式参数名与路由参数名不一致时，
// 以显式为准（读不到值 ⇒ 400 invalid id，缺省文案逐字不变）。
func TestGet_IDParamExplicit_OverridesDerived(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc,
		WithIDParam[Item, int64, CreateItemReq, UpdateItemReq]("pk"))
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, "{\"code\":400,\"data\":null,\"message\":\"invalid id\",\"trace_id\":\"\"}", w.Body.String())
}

// TestGet_DefaultIDParam_Unchanged 缺省仍读 "id"（逐字不变）。
func TestGet_DefaultIDParam_Unchanged(t *testing.T) {
	h, svc := newHandler()
	if _, err := svc.Create(context.Background(), &CreateItemReq{Name: "found"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	engine := newEngineWith(t, h, "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items/1", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
}

// TestDeriveIDParam_ConflictFallsBackToId 多条 CRUD 路由参数名不一致 ⇒ 回退 "id"。
func TestDeriveIDParam_ConflictFallsBackToId(t *testing.T) {
	assert.DeepEqual(t, "id", deriveIDParam([]Route{
		{Method: consts.MethodGet, Path: "/:code", Op: OpGet},
		{Method: consts.MethodPut, Path: "/:id", Op: OpUpdate},
	}))
	assert.DeepEqual(t, "code", deriveIDParam([]Route{
		{Method: consts.MethodGet, Path: "/:code", Op: OpGet},
		{Method: consts.MethodPut, Path: "/:code", Op: OpUpdate},
	}))
	// List / 自定义端点不参与推导
	assert.DeepEqual(t, "id", deriveIDParam([]Route{
		{Method: consts.MethodGet, Path: "/:type/items", Op: OpList},
	}))
	// 缺省路由 ⇒ "id"
	assert.DeepEqual(t, "id", deriveIDParam(nil))
}
