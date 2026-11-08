package crudh

import (
	"context"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
)

// --- #165 回归：排序白名单对齐 DB 真实列名，出 SQL 恒用规范化列名 ---
//
// v0.9.0~v0.10.6 的白名单收「Go 字段名小写 + json 名 + gorm column tag」，
// 但无 column tag 字段的 DB 真列名是 NamingStrategy 蛇形（DisplayName →
// display_name）：按 json 名 displayname 排序**通过白名单校验**后，用户原文
// "displayname" 直接拼进 ORDER BY ⇒ 列不存在 ⇒ DB 报错 5xx。v0.9.0 的卖点
// 「不再让不存在的列进 ORDER BY」只堵住了反向缺口（真列名被拒），正向缺口
// （假列名放行）未覆盖。
//
// 修复后：白名单是「别名 → 规范化真列名」映射，校验查别名、出 SQL 用
// 规范化名，用户原文不再进 ORDER BY。

// sortAliasEntity 覆盖 #165 的列名来源矩阵：无 tag 字段（蛇形真列名）、
// json 名与真列名不同形（别名必须映射）、gorm column tag 覆写、缩写词字段。
type sortAliasEntity struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"displayName"` // 无 gorm tag：真列名 display_name
	UserID      int64  // 无任何 tag：真列名 user_id（缩写词）
	SecretCol   string `json:"secret" gorm:"column:secret_col"`
}

type sortAliasReq struct {
	DisplayName string `json:"displayName"`
}

// orderSpyService 捕获 List 收到的 *crud.Query，断言最终进 SQL 的 ORDER BY。
type orderSpyService struct {
	mu     sync.Mutex
	orders []string
}

func (s *orderSpyService) Create(_ context.Context, _ *sortAliasReq) (*sortAliasEntity, error) {
	return &sortAliasEntity{}, nil
}

func (s *orderSpyService) Get(_ context.Context, _ int64) (*sortAliasEntity, error) {
	return &sortAliasEntity{}, nil
}

func (s *orderSpyService) GetOne(_ context.Context, _ *crud.Query) (*sortAliasEntity, error) {
	return &sortAliasEntity{}, nil
}

func (s *orderSpyService) List(_ context.Context, q *crud.Query) (*crud.PageResponse[sortAliasEntity], error) {
	s.mu.Lock()
	s.orders = q.Spec().Orders
	s.mu.Unlock()
	return &crud.PageResponse[sortAliasEntity]{Items: []sortAliasEntity{}, Total: 0}, nil
}

func (s *orderSpyService) Update(_ context.Context, _ int64, _ *sortAliasReq) (*sortAliasEntity, error) {
	return &sortAliasEntity{}, nil
}

func (s *orderSpyService) Delete(_ context.Context, _ int64) error { return nil }

func (s *orderSpyService) capturedOrders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.orders
}

// newAliasSortEngine 装配 #165 实体的 List 端点。
func newAliasSortEngine(t *testing.T, svc *orderSpyService) *route.Engine {
	t.Helper()
	h := NewHandler[sortAliasEntity, int64, sortAliasReq, sortAliasReq](svc)
	engine := route.NewEngine(config.NewOptions(nil))
	h.RegisterRoutes(engine.Group("/api"), "/items")
	return engine
}

// TestList_Sort165_AliasEmitsCanonicalColumn 端到端钉 #165：合法别名（含
// json 名、蛇形真列名、大小写变体）全部放行，且 Query.Orders 里出现的是
// 规范化真列名 + 规范化方向——用户原文不再进 ORDER BY。
func TestList_Sort165_AliasEmitsCanonicalColumn(t *testing.T) {
	tests := []struct {
		name       string
		sort       string
		wantCode   int
		wantOrders []string
	}{
		{
			name:       "json alias lower maps to snake column",
			sort:       "displayname",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"display_name"},
		},
		{
			name:       "json alias exact camel also maps to snake column",
			sort:       "displayName",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"display_name"},
		},
		{
			name:       "snake column itself accepted",
			sort:       "display_name",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"display_name"},
		},
		{
			name:       "case insensitive input emits canonical spelling",
			sort:       "DISPLAY_NAME DESC",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"display_name DESC"},
		},
		{
			name:       "direction lowercased input normalized to upper",
			sort:       "display_name desc",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"display_name DESC"},
		},
		{
			name:       "untagged initialism field by snake name",
			sort:       "user_id",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"user_id"},
		},
		{
			name:       "gorm column tag column",
			sort:       "secret_col",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"secret_col"},
		},
		{
			name:       "json alias maps to gorm column tag column",
			sort:       "secret",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"secret_col"},
		},
		{
			name:       "multi segment mixed aliases",
			sort:       "displayName,user_id desc",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"display_name", "user_id DESC"},
		},
		{
			name:     "go field name no longer accepted (displayname for untagged UserID)",
			sort:     "userid",
			wantCode: consts.StatusBadRequest,
		},
		{
			name:     "go field name exact case no longer accepted",
			sort:     "UserID",
			wantCode: consts.StatusBadRequest,
		},
		{
			name:     "unknown column still 400",
			sort:     "nosuchcolumn",
			wantCode: consts.StatusBadRequest,
		},
		{
			name:     "injection-ish identifier still 400 via whitelist miss",
			sort:     "(SELECT CASE WHEN",
			wantCode: consts.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &orderSpyService{}
			engine := newAliasSortEngine(t, svc)

			w := doJSON(t, engine, consts.MethodGet, "/api/items?sort="+tt.sort, "")
			assert.DeepEqual(t, tt.wantCode, w.Code)
			if tt.wantCode != consts.StatusOK {
				return
			}
			got := svc.capturedOrders()
			assert.DeepEqual(t, tt.wantOrders, got)
		})
	}
}

// TestList_Sort165_ExplicitWhitelistPassesCallerSpelling 显式白名单（调用方
// 声明的真实列名）按调用方书写形式出 SQL——规范化名即调用方给的列名。
func TestList_Sort165_ExplicitWhitelistPassesCallerSpelling(t *testing.T) {
	svc := &orderSpyService{}
	h := NewHandler[sortAliasEntity, int64, sortAliasReq, sortAliasReq](svc,
		WithSortableColumns[sortAliasEntity, int64, sortAliasReq, sortAliasReq]("display_name"))
	engine := route.NewEngine(config.NewOptions(nil))
	h.RegisterRoutes(engine.Group("/api"), "/items")

	w := doJSON(t, engine, consts.MethodGet, "/api/items?sort=Display_Name+Desc", "")
	assert.DeepEqual(t, consts.StatusOK, w.Code)
	assert.DeepEqual(t, []string{"display_name DESC"}, svc.capturedOrders())

	// 白名单外的列照旧 400
	w = doJSON(t, engine, consts.MethodGet, "/api/items?sort=secret_col", "")
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)
}
