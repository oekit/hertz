package crudh

import (
	"context"
	"database/sql/driver"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
)

// --- #175 回归：排序白名单展开嵌入结构体的内层字段，嵌入类型名不是列 ---
//
// 此前 collectEntityColumns 对匿名字段无条件按「字段名蛇形 = 列名」入表：
// gorm.Model 嵌入产出幽灵列 "model"（gorm 只提升 ID/CreatedAt 等内层字段，
// 不为嵌入类型建列）——?sort=model 通过白名单校验后 ORDER BY model ⇒ DB 报
// no such column ⇒ 5xx（#165 要消灭的形态换道复活）；反向地，具名字段的
// gorm:"embedded" 不被识别：真列 author_name 被误拒成 400，幽灵列 author 放行。

// sweepBase 与 sweepEntity 复刻 issue 的触发实体：gorm.Model + 匿名公共基类。
type sweepBase struct {
	CreatedAt time.Time
}

type sweepEntity struct {
	gorm.Model
	sweepBase
	Name string `json:"name"`
}

func TestEntityColumnsSkipsEmbeddedTypeName(t *testing.T) {
	cols := entityColumns[sweepEntity]()
	if _, ok := cols["model"]; ok {
		t.Fatal("嵌入类型名（gorm.Model → model）不应作为可排序列：gorm 不为嵌入类型建列")
	}
	if _, ok := cols["base"]; ok {
		t.Fatal("嵌入类型名（sweepBase → base）不应作为可排序列")
	}
	// 嵌入结构体的内层字段是真实列，必须仍可排序（gorm.Model 的 ID/CreatedAt
	// 等提升列 + 基类的 CreatedAt/Name）。
	for _, want := range []string{"id", "created_at", "updated_at", "deleted_at", "name"} {
		if cols[want] == "" {
			t.Errorf("whitelist missing promoted column %q, got %v", want, keysOf(cols))
		}
	}
}

// embAuthor 走具名字段的 gorm:"embedded"（gorm 以标签表达嵌入，非匿名），
// 内层列被 embeddedPrefix 加前缀。
type embAuthor struct {
	Name  string    `json:"name"`                           // 真列 author_name，json 别名 name
	Email string    `json:"email" gorm:"column:email_addr"` // 真列 author_email_addr（tag + 前缀），json 别名 email
	Age   int       `gorm:"-"`                              // 忽略字段：不是列
	Since time.Time // 真列 author_since
}

type embArticle struct {
	ID     int64     `json:"id"`
	Title  string    `json:"title"`
	Author embAuthor `gorm:"embedded;embeddedPrefix:author_"`
}

func TestEntityColumnsExpandsEmbeddedTag(t *testing.T) {
	cols := entityColumns[embArticle]()
	if _, ok := cols["author"]; ok {
		t.Fatal("具名字段的 gorm:\"embedded\" 同样不应把字段名/类型名当列")
	}
	for _, want := range []string{"id", "title", "author_name", "author_email_addr", "author_since"} {
		if cols[want] == "" {
			t.Errorf("whitelist missing embedded column %q, got %v", want, keysOf(cols))
		}
	}
	// embeddedPrefix 拼进内层列（含 column tag 覆写的）：裸内层名不是列。
	if _, ok := cols["email_addr"]; ok {
		t.Error("prefixed embedded field must not register the unprefixed column")
	}
	// 内层 json 别名映射到加前缀的规范化列。
	if cols["name"] != "author_name" {
		t.Errorf("alias %q → %q, want author_name", "name", cols["name"])
	}
	if cols["email"] != "author_email_addr" {
		t.Errorf("alias %q → %q, want author_email_addr", "email", cols["email"])
	}
	// gorm:"-" 的内层字段不是列。
	if _, ok := cols["age"]; ok {
		t.Error("gorm:\"-\" inner field must not be sortable")
	}
}

// embMoney 实现 driver.Valuer（指针接收者）：匿名嵌入时 gorm 不展开内层字段，
// 而是把该字段当单列。
type embMoney struct {
	Cents int64
}

func (m *embMoney) Value() (driver.Value, error) { return m.Cents, nil }

type embGadget struct {
	Secret    string   `gorm:"-"`
	Internal  string   `gorm:"-:all"`
	Migrated  string   `gorm:"-:migration"` // 仅不参与建表迁移，仍是真实列
	Price     embMoney // 匿名 Valuer ⇒ 单列 price（内层 cents 不是列）
	time.Time          // 匿名 time.Time ⇒ 单列 time（gorm 同口径）
	Name      string   `json:"name"`
}

func TestEntityColumnsValuerAndIgnoredFields(t *testing.T) {
	cols := entityColumns[embGadget]()
	if _, ok := cols["secret"]; ok {
		t.Error("gorm:\"-\" field must not be sortable")
	}
	if _, ok := cols["internal"]; ok {
		t.Error("gorm:\"-:all\" field must not be sortable")
	}
	if cols["migrated"] == "" {
		t.Error("gorm:\"-:migration\" field is still a real column (migration-only ignore)")
	}
	if cols["price"] == "" {
		t.Errorf("anonymous Valuer must map to a single column %q, got %v", "price", keysOf(cols))
	}
	if _, ok := cols["cents"]; ok {
		t.Error("anonymous Valuer inner fields are not columns")
	}
	if cols["time"] == "" {
		t.Error("anonymous time.Time maps to single column \"time\" (gorm parity)")
	}
	if _, ok := cols["stamp"]; ok {
		t.Error("no field named Stamp exists")
	}
	if cols["name"] == "" {
		t.Error("plain field must stay sortable")
	}
}

// embOrder / embItem 验证嵌套嵌入的前缀累加。
type embItem struct {
	SKU string `json:"sku"`
}

type embOrder struct {
	embItem         // 无前缀：sku
	Line    embItem `gorm:"embedded;embeddedPrefix:line_"` // line_sku
}

func TestEntityColumnsNestedEmbeddedPrefixAccumulates(t *testing.T) {
	cols := entityColumns[embOrder]()
	if cols["sku"] == "" || cols["line_sku"] == "" {
		t.Errorf("nested embedded prefixes must accumulate, got %v", keysOf(cols))
	}
	if _, ok := cols["line"]; ok {
		t.Error("embedded type name must not become a column")
	}
}

// keysOf 排序返回白名单键（错误信息可读）。
func keysOf(cols map[string]string) []string {
	out := make([]string, 0, len(cols))
	for k := range cols {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// --- 端到端：嵌入实体的 List 排序 ---

type embSortReq struct {
	Title string `json:"title"`
}

// embSpyService 捕获 List 收到的 ORDER BY（按实体类型实例化）。
type embSpyService[T any] struct {
	mu     sync.Mutex
	orders []string
}

func (s *embSpyService[T]) Create(_ context.Context, _ *embSortReq) (*T, error) {
	return new(T), nil
}

func (s *embSpyService[T]) Get(_ context.Context, _ int64) (*T, error) {
	return new(T), nil
}

func (s *embSpyService[T]) GetOne(_ context.Context, _ *crud.Query) (*T, error) {
	return new(T), nil
}

func (s *embSpyService[T]) List(_ context.Context, q *crud.Query) (*crud.PageResponse[T], error) {
	s.mu.Lock()
	s.orders = q.Spec().Orders
	s.mu.Unlock()
	return &crud.PageResponse[T]{Items: []T{}, Total: 0}, nil
}

func (s *embSpyService[T]) Update(_ context.Context, _ int64, _ *embSortReq) (*T, error) {
	return new(T), nil
}

func (s *embSpyService[T]) Delete(_ context.Context, _ int64) error { return nil }

// TestList_Sort175_EmbeddedColumnsEndToEnd 端到端钉 #175：嵌入展开后的真实列
// （含 embeddedPrefix 前缀列与内层 json 别名）放行且出规范化列名；嵌入类型名
// 幽灵列被 400 拒绝——不再进入 ORDER BY。
func TestList_Sort175_EmbeddedColumnsEndToEnd(t *testing.T) {
	tests := []struct {
		name       string
		sort       string
		wantCode   int
		wantOrders []string
	}{
		{
			name:       "embedded prefixed column accepted",
			sort:       "author_name",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"author_name"},
		},
		{
			name:       "inner json alias maps to prefixed column",
			sort:       "name",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"author_name"},
		},
		{
			name:       "prefixed column-tag column accepted",
			sort:       "author_email_addr desc",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"author_email_addr DESC"},
		},
		{
			name:       "top-level column still accepted",
			sort:       "title",
			wantCode:   consts.StatusOK,
			wantOrders: []string{"title"},
		},
		{
			name:     "embedded type name is rejected (no ghost column)",
			sort:     "author",
			wantCode: consts.StatusBadRequest,
		},
		{
			name:     "unprefixed inner column name rejected",
			sort:     "email_addr",
			wantCode: consts.StatusBadRequest,
		},
		{
			name:     "gorm-model style ghost column rejected (issue trigger)",
			sort:     "model",
			wantCode: consts.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &embSpyService[embArticle]{}
			h := NewHandler[embArticle, int64, embSortReq, embSortReq](svc)
			engine := route.NewEngine(config.NewOptions(nil))
			h.RegisterRoutes(engine.Group("/api"), "/items")

			w := doJSON(t, engine, consts.MethodGet, "/api/items?sort="+tt.sort, "")
			assert.DeepEqual(t, tt.wantCode, w.Code)
			if tt.wantCode != consts.StatusOK {
				return
			}
			assert.DeepEqual(t, tt.wantOrders, svc.orders)
		})
	}
}

// TestList_Sort175_ModelEntityGhostColumnRejected 触发路径原样复刻：gorm.Model
// 嵌入实体、缺省白名单，?sort=model 必须 400（修复前放行 "model" 进 ORDER BY ⇒ 5xx）。
func TestList_Sort175_ModelEntityGhostColumnRejected(t *testing.T) {
	h := NewHandler[sweepEntity, int64, embSortReq, embSortReq](&embSpyService[sweepEntity]{})
	engine := route.NewEngine(config.NewOptions(nil))
	h.RegisterRoutes(engine.Group("/api"), "/items")

	w := ut.PerformRequest(engine, consts.MethodGet, "/api/items?sort=model", nil)
	assert.DeepEqual(t, consts.StatusBadRequest, w.Code)

	// 内层真实列照常可排。
	w = ut.PerformRequest(engine, consts.MethodGet, "/api/items?sort=created_at", nil)
	assert.DeepEqual(t, consts.StatusOK, w.Code)
}
