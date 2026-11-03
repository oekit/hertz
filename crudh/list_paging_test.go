package crudh

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
	bizerrors "github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/pagination"
)

// --- v0.9.0 改造①：List 缺省分页「有界 + 归一」---
//
// 判据（每条都对着 gormx 真实回显逻辑与 pkg/pagination 真实 LIMIT）：
//   - 缺省请求（无 page/page_size）必须带 LIMIT（不再全表扫描 + 全量序列化）；
//   - 响应回显的 page/page_size 必须**恒等于** SQL 实际 LIMIT（回显不许说谎）；
//   - v0.8.14 逐字口径必须可用开关保留（WithPageNormalization(false)）；
//   - WithListParams(false) 的既有契约不变（忽略参数、恒全量、回显 0）。

// pagingRepo 是最小 crud.Repository 实现：List 用查询自带的分页参数截断，
// **取值与 gormrepo SQL 层完全一致**（p.Offset()/p.Limit()），并记录最近一次
// 查询与实际生效的 LIMIT/OFFSET；其余方法仅满足接口。
type pagingRepo struct {
	mu         sync.Mutex
	items      []Item
	lastQuery  *crud.Query
	lastLimit  int
	lastOffset int
}

func (r *pagingRepo) Create(_ context.Context, entity *Item) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, *entity)
	return nil
}

func (r *pagingRepo) BatchCreate(_ context.Context, entities []*Item, _ int) error {
	for _, e := range entities {
		if err := r.Create(context.Background(), e); err != nil {
			return err
		}
	}
	return nil
}

func (r *pagingRepo) Update(_ context.Context, entity *Item, _ ...crud.UpdateOption) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.items {
		if r.items[i].ID == entity.ID {
			r.items[i] = *entity
			return 1, nil
		}
	}
	return 0, nil
}

func (r *pagingRepo) UpdateFields(_ context.Context, id int64, fields map[string]any) (int64, error) {
	return 0, nil
}

func (r *pagingRepo) Delete(_ context.Context, id int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.items {
		if r.items[i].ID == id {
			r.items = append(r.items[:i], r.items[i+1:]...)
			return 1, nil
		}
	}
	return 0, nil
}

func (r *pagingRepo) BatchDelete(_ context.Context, ids []int64) (int64, error) {
	return 0, nil
}

func (r *pagingRepo) GetByID(_ context.Context, id int64) (*Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.items {
		if r.items[i].ID == id {
			it := r.items[i]
			return &it, nil
		}
	}
	return nil, bizerrors.NewBizError(404, "not found")
}

func (r *pagingRepo) GetOne(_ context.Context, _ *crud.Query) (*Item, error) {
	return nil, crud.ErrNotFound
}

func (r *pagingRepo) GetByCondition(_ context.Context, _ any, _ ...any) (*Item, error) {
	return nil, crud.ErrNotFound
}

// List 按分页参数截断（模拟 SQL 的 OFFSET/LIMIT）。未分页（q 无 Page）时全量返回
// ——这正是 v0.8.14 缺省请求的行为（SQL 无 LIMIT ⇒ 全表扫描）。
func (r *pagingRepo) List(_ context.Context, q *crud.Query) ([]Item, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastQuery = q
	total := int64(len(r.items))
	if q == nil || q.GetPage() == nil {
		r.lastLimit, r.lastOffset = 0, 0
		out := make([]Item, len(r.items))
		copy(out, r.items)
		return out, total, nil
	}
	p := q.GetPage()
	offset, limit := p.Offset(), p.Limit()
	r.lastLimit, r.lastOffset = limit, offset
	if offset >= len(r.items) {
		return []Item{}, total, nil
	}
	end := offset + limit
	if end > len(r.items) {
		end = len(r.items)
	}
	out := make([]Item, end-offset)
	copy(out, r.items[offset:end])
	return out, total, nil
}

func (r *pagingRepo) Exists(_ context.Context, _ *crud.Query) (bool, error) { return false, nil }

func (r *pagingRepo) Count(_ context.Context, _ *crud.Query) (int64, error) { return 0, nil }

func (r *pagingRepo) snapshot() (q *crud.Query, limit, offset int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastQuery, r.lastLimit, r.lastOffset
}

// newPagingHandler 用**真实** crud.BaseService（回显逻辑与生产一致）构造 Handler。
func newPagingHandler(t *testing.T, n int, opts ...HandlerOption[Item, int64, CreateItemReq, UpdateItemReq]) (*Handler[Item, int64, CreateItemReq, UpdateItemReq], *pagingRepo) {
	t.Helper()
	repo := &pagingRepo{}
	for i := 1; i <= n; i++ {
		repo.items = append(repo.items, Item{ID: int64(i), Name: "item"})
	}
	svc := crud.NewBaseService[Item, int64, CreateItemReq, UpdateItemReq](repo)
	return NewHandler[Item, int64, CreateItemReq, UpdateItemReq](svc, opts...), repo
}

// pageData 是列表响应的 data 结构（crud.PageResponse = pagination.PageResponse）。
type pageData struct {
	Items    []Item `json:"items"`
	Total    int64  `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Pages    int    `json:"pages"`
}

// routeEngine 把 Handler 挂到引擎上并记住仓储（供断言 SQL 实际 LIMIT）。
type routeEngine struct {
	engine *route.Engine
	repo   *pagingRepo
}

func newPagingEngine(t *testing.T, h *Handler[Item, int64, CreateItemReq, UpdateItemReq], repo *pagingRepo) *routeEngine {
	t.Helper()
	engine := route.NewEngine(config.NewOptions(nil))
	h.RegisterRoutes(engine.Group("/api"), "/items")
	return &routeEngine{engine: engine, repo: repo}
}

// listPage 发起一次列表请求并解析分页信封。
func listPage(t *testing.T, engine *routeEngine, query string) pageData {
	t.Helper()
	w := doJSON(t, engine.engine, consts.MethodGet, "/api/items"+query, "")
	if w.Code != consts.StatusOK {
		t.Fatalf("list%s: expected 200, got %d body=%s", query, w.Code, w.Body.String())
	}
	resp := decodeEnvelope(t, w)
	var pd pageData
	if err := json.Unmarshal(resp.Data, &pd); err != nil {
		t.Fatalf("unmarshal page data %s: %v", resp.Data, err)
	}
	return pd
}

// TestList_DefaultPaging_IsBounded 改造①缺省行为：缺省请求（无 page/page_size）
// 必须带 LIMIT（v0.8.14 缺省无 LIMIT ⇒ 全表扫描 + 全量序列化），且回显 1/20。
func TestList_DefaultPaging_IsBounded(t *testing.T) {
	h, repo := newPagingHandler(t, 3)
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "")
	assert.DeepEqual(t, 1, pd.Page)
	assert.DeepEqual(t, pagination.DefaultPageSize, pd.PageSize)
	assert.DeepEqual(t, 1, pd.Pages)
	assert.DeepEqual(t, 3, len(pd.Items))

	_, limit, offset := repo.snapshot()
	assert.DeepEqual(t, pagination.DefaultPageSize, limit)
	assert.DeepEqual(t, 0, offset)
}

// TestList_DefaultPaging_BoundedWhenManyRows 缺省请求只取首屏（items 截断到 LIMIT）。
func TestList_DefaultPaging_BoundedWhenManyRows(t *testing.T) {
	h, repo := newPagingHandler(t, 100)
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "")
	assert.DeepEqual(t, pagination.DefaultPageSize, len(pd.Items))
	assert.DeepEqual(t, int64(100), pd.Total)
	assert.DeepEqual(t, 5, pd.Pages)

	_, limit, _ := repo.snapshot()
	assert.DeepEqual(t, pagination.DefaultPageSize, limit)
}

// TestList_PageSizeOverLimit_EchoEqualsActualLimit 改造①核心判据：
// page_size=100000 时实际 LIMIT 是 500（pkg 封顶），回显必须也是 500，
// pages 按 500 计算——v0.8.14 回显未归一的 100000（前端算出的页数与实收数据矛盾）。
func TestList_PageSizeOverLimit_EchoEqualsActualLimit(t *testing.T) {
	h, repo := newPagingHandler(t, 1000)
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "?page_size=100000")
	assert.DeepEqual(t, pagination.MaxPageSize, pd.PageSize)
	assert.DeepEqual(t, 2, pd.Pages) // ceil(1000/500)
	assert.DeepEqual(t, pagination.MaxPageSize, len(pd.Items))

	_, limit, _ := repo.snapshot()
	assert.DeepEqual(t, pagination.MaxPageSize, limit)
}

// TestList_PageSizeZero_EchoEqualsActualLimit page_size=0 时实际 LIMIT 是缺省 20，
// 回显必须也是 20（v0.8.14 回显 0、pages=0，与实收数据矛盾）。
func TestList_PageSizeZero_EchoEqualsActualLimit(t *testing.T) {
	h, repo := newPagingHandler(t, 100)
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "?page=1&page_size=0")
	assert.DeepEqual(t, 1, pd.Page)
	assert.DeepEqual(t, pagination.DefaultPageSize, pd.PageSize)
	assert.DeepEqual(t, 5, pd.Pages)

	_, limit, _ := repo.snapshot()
	assert.DeepEqual(t, pagination.DefaultPageSize, limit)
}

// TestList_NonPositivePage_NormalizedToFirstPage page<=0 归一为第 1 页（偏移 0）。
func TestList_NonPositivePage_NormalizedToFirstPage(t *testing.T) {
	h, repo := newPagingHandler(t, 10)
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "?page=0&page_size=5")
	assert.DeepEqual(t, 1, pd.Page)
	assert.DeepEqual(t, 5, pd.PageSize)

	_, limit, offset := repo.snapshot()
	assert.DeepEqual(t, 5, limit)
	assert.DeepEqual(t, 0, offset)
}

// TestList_PageOffset_Applied 分页偏移按归一后的页码计算。
func TestList_PageOffset_Applied(t *testing.T) {
	h, repo := newPagingHandler(t, 30)
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "?page=3&page_size=10")
	assert.DeepEqual(t, 3, pd.Page)
	assert.DeepEqual(t, 10, pd.PageSize)
	assert.DeepEqual(t, 3, pd.Pages)
	assert.DeepEqual(t, 10, len(pd.Items))
	assert.DeepEqual(t, int64(21), pd.Items[0].ID) // 第 3 页第 1 条 = ID 21

	_, limit, offset := repo.snapshot()
	assert.DeepEqual(t, 10, limit)
	assert.DeepEqual(t, 20, offset)
}

// TestList_DefaultPageSize_Option WithDefaultPageSize 生效：缺省请求按配置的
// 每页条数分页，且回显与实际 LIMIT 一致。
func TestList_DefaultPageSize_Option(t *testing.T) {
	h, repo := newPagingHandler(t, 100,
		WithDefaultPageSize[Item, int64, CreateItemReq, UpdateItemReq](50))
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "")
	assert.DeepEqual(t, 50, pd.PageSize)
	assert.DeepEqual(t, 2, pd.Pages)
	assert.DeepEqual(t, 50, len(pd.Items))

	_, limit, _ := repo.snapshot()
	assert.DeepEqual(t, 50, limit)
}

// TestList_PageSizeLimit_Option WithPageSizeLimit 生效：超限的 page_size 按上限
// 截断，回显与实际 LIMIT 一致。
func TestList_PageSizeLimit_Option(t *testing.T) {
	h, repo := newPagingHandler(t, 1000,
		WithPageSizeLimit[Item, int64, CreateItemReq, UpdateItemReq](100))
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "?page_size=100000")
	assert.DeepEqual(t, 100, pd.PageSize)
	assert.DeepEqual(t, 10, pd.Pages)
	assert.DeepEqual(t, 100, len(pd.Items))

	_, limit, _ := repo.snapshot()
	assert.DeepEqual(t, 100, limit)
}

// TestList_PageSizeLimit_NotAbovePackageMax 上限不能突破 pkg 的 500 硬顶
// （下游 Limit() 封顶）——按 500 生效，回显与实际 LIMIT 恒等。
func TestList_PageSizeLimit_NotAbovePackageMax(t *testing.T) {
	h, repo := newPagingHandler(t, 1000,
		WithPageSizeLimit[Item, int64, CreateItemReq, UpdateItemReq](100000))
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "?page_size=100000")
	assert.DeepEqual(t, pagination.MaxPageSize, pd.PageSize)

	_, limit, _ := repo.snapshot()
	assert.DeepEqual(t, pagination.MaxPageSize, limit)
}

// TestList_PageNormalizationDisabled_KeepsV0814 「保留旧行为」开关：
// WithPageNormalization(false) 时行为与 v0.8.14 **逐字一致**——
//   - 缺省请求不分页（SQL 无 LIMIT，回显 0/0/0）；
//   - page=1&page_size=0 → 回显 page_size=0、pages=0；
//   - page_size=100000 → 回显 100000（未归一）。
func TestList_PageNormalizationDisabled_KeepsV0814(t *testing.T) {
	h, repo := newPagingHandler(t, 1000,
		WithPageNormalization[Item, int64, CreateItemReq, UpdateItemReq](false))
	engine := newPagingEngine(t, h, repo)

	// 缺省请求：不分页（q 无 Page ⇒ SQL 无 LIMIT），回显全 0
	pd := listPage(t, engine, "")
	assert.DeepEqual(t, 0, pd.Page)
	assert.DeepEqual(t, 0, pd.PageSize)
	assert.DeepEqual(t, 0, pd.Pages)
	assert.DeepEqual(t, 1000, len(pd.Items))
	_, limit, _ := repo.snapshot()
	assert.DeepEqual(t, 0, limit)

	// page=1&page_size=0：调了 Page 但不归一 ⇒ 回显 page=1、page_size=0、pages=0
	pd = listPage(t, engine, "?page=1&page_size=0")
	assert.DeepEqual(t, 1, pd.Page)
	assert.DeepEqual(t, 0, pd.PageSize)
	assert.DeepEqual(t, 0, pd.Pages)

	// page_size=100000：回显未归一的原值（v0.8.14 逐字）
	pd = listPage(t, engine, "?page_size=100000")
	assert.DeepEqual(t, 0, pd.Page)
	assert.DeepEqual(t, 100000, pd.PageSize)
	assert.DeepEqual(t, 1, pd.Pages)
}

// TestListParamsDisabled_KeepsZeroEcho WithListParams(false) 的既有契约不变：
// 忽略 page/page_size/sort、恒全量返回、回显 Page/PageSize 保持 0
// （不套用新归一口径）。
func TestListParamsDisabled_KeepsZeroEcho(t *testing.T) {
	h, repo := newPagingHandler(t, 1000,
		WithListParams[Item, int64, CreateItemReq, UpdateItemReq](false))
	engine := newPagingEngine(t, h, repo)

	pd := listPage(t, engine, "?page=2&page_size=10")
	assert.DeepEqual(t, 0, pd.Page)
	assert.DeepEqual(t, 0, pd.PageSize)
	assert.DeepEqual(t, 0, pd.Pages)
	assert.DeepEqual(t, 1000, len(pd.Items))

	q, limit, _ := repo.snapshot()
	assert.NotNil(t, q)
	assert.Nil(t, q.GetPage())
	assert.DeepEqual(t, 0, limit)
}

// TestList_InvalidPageParam_400 非法分页值仍逐字 400（口径不变）。
func TestList_InvalidPageParam_400(t *testing.T) {
	h, repo := newPagingHandler(t, 1)
	engine := newPagingEngine(t, h, repo)

	w := doJSON(t, engine.engine, consts.MethodGet, "/api/items?page=x", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, "{\"code\":400,\"data\":null,\"message\":\"invalid page parameter\",\"trace_id\":\"\"}", w.Body.String())

	w = doJSON(t, engine.engine, consts.MethodGet, "/api/items?page_size=y", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
	assert.DeepEqual(t, "{\"code\":400,\"data\":null,\"message\":\"invalid page_size parameter\",\"trace_id\":\"\"}", w.Body.String())
}
