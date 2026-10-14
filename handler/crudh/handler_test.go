package crudh

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/cloudwego/hertz/pkg/route/param"
	bizerrors "github.com/yi-nology/ekit-core/pkg/errors"
	"github.com/yi-nology/ekit-gorm/crud"
)

// --- Test domain types ---

type Item struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type CreateItemReq struct {
	Name string `json:"name" validate:"required"`
}

type UpdateItemReq struct {
	Name string `json:"name" validate:"required"`
}

// --- Mock Service ---

type mockItemService struct {
	mu        sync.Mutex
	items     map[int64]*Item
	nextID    int64
	createErr error
	getErr    error
	updateErr error
	deleteErr error
	listErr   error
}

func newMockItemService() *mockItemService {
	return &mockItemService{
		items:  make(map[int64]*Item),
		nextID: 1,
	}
}

func (m *mockItemService) Create(_ context.Context, req *CreateItemReq) (*Item, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item := &Item{ID: m.nextID, Name: req.Name}
	m.items[m.nextID] = item
	m.nextID++
	return item, nil
}

func (m *mockItemService) Get(_ context.Context, id int64) (*Item, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[id]
	if !ok {
		return nil, bizerrors.NewBizError(404, "not found")
	}
	return item, nil
}

func (m *mockItemService) GetOne(_ context.Context, _ *crud.Query) (*Item, error) {
	return nil, nil
}

func (m *mockItemService) List(_ context.Context, q *crud.Query) (*crud.PageResponse[Item], error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]Item, 0, len(m.items))
	for _, item := range m.items {
		list = append(list, *item)
	}
	return &crud.PageResponse[Item]{
		Items:    list,
		Total:    int64(len(list)),
		Page:     1,
		PageSize: 20,
	}, nil
}

func (m *mockItemService) Update(_ context.Context, id int64, req *UpdateItemReq) (*Item, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[id]
	if !ok {
		return nil, bizerrors.NewBizError(404, "not found")
	}
	item.Name = req.Name
	return item, nil
}

func (m *mockItemService) Delete(_ context.Context, id int64) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	return nil
}

// --- Helpers ---

type apiResponse struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func setJSONBody(c *app.RequestContext, body string) {
	c.Request.Header.SetContentTypeBytes([]byte("application/json"))
	c.Request.SetBodyStream(bytes.NewReader([]byte(body)), len(body))
}

func setPathParam(c *app.RequestContext, key, value string) {
	c.Params = param.Params{{Key: key, Value: value}}
}

func newHandler() (*Handler[Item, CreateItemReq, UpdateItemReq], *mockItemService) {
	svc := newMockItemService()
	return NewHandler[Item, CreateItemReq, UpdateItemReq](svc), svc
}

func parseResp(t *testing.T, c *app.RequestContext) apiResponse {
	t.Helper()
	var resp apiResponse
	if err := json.Unmarshal(c.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response body %q: %v", c.Response.Body(), err)
	}
	return resp
}

// --- Create tests ---

func TestCreate_Success(t *testing.T) {
	h, _ := newHandler()
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{"name":"widget"}`)

	h.Create(context.Background(), c)

	assert.DeepEqual(t, consts.StatusOK, c.Response.StatusCode())
	resp := parseResp(t, c)
	assert.DeepEqual(t, 0, resp.Code)

	var item Item
	_ = json.Unmarshal(resp.Data, &item)
	assert.DeepEqual(t, "widget", item.Name)
	if item.ID <= 0 {
		t.Errorf("expected positive ID, got %d", item.ID)
	}
}

func TestCreate_ValidationError_EmptyName(t *testing.T) {
	h, _ := newHandler()
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{"name":""}`)

	h.Create(context.Background(), c)

	resp := parseResp(t, c)
	if resp.Code == 0 {
		t.Error("expected non-zero error code for validation failure")
	}
}

func TestCreate_ServiceError(t *testing.T) {
	h, svc := newHandler()
	svc.createErr = bizerrors.NewBizError(500, "db down")
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{"name":"widget"}`)

	h.Create(context.Background(), c)

	resp := parseResp(t, c)
	assert.DeepEqual(t, 500, resp.Code)
}

// --- Get tests ---

func TestGet_Success(t *testing.T) {
	h, svc := newHandler()
	item, _ := svc.Create(context.Background(), &CreateItemReq{Name: "found"})

	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	setPathParam(c, "id", "1")

	h.Get(context.Background(), c)

	assert.DeepEqual(t, consts.StatusOK, c.Response.StatusCode())
	resp := parseResp(t, c)
	assert.DeepEqual(t, 0, resp.Code)

	var got Item
	_ = json.Unmarshal(resp.Data, &got)
	assert.DeepEqual(t, item.ID, got.ID)
	assert.DeepEqual(t, "found", got.Name)
}

func TestGet_InvalidID(t *testing.T) {
	h, _ := newHandler()
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	setPathParam(c, "id", "abc")

	h.Get(context.Background(), c)

	assert.DeepEqual(t, consts.StatusBadRequest, c.Response.StatusCode())
}

func TestGet_NotFound(t *testing.T) {
	h, _ := newHandler()
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	setPathParam(c, "id", "999")

	h.Get(context.Background(), c)

	resp := parseResp(t, c)
	assert.DeepEqual(t, 404, resp.Code)
}

// --- List tests ---

func TestList_Success(t *testing.T) {
	h, svc := newHandler()
	_, _ = svc.Create(context.Background(), &CreateItemReq{Name: "a"})
	_, _ = svc.Create(context.Background(), &CreateItemReq{Name: "b"})

	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	c.Request.URI().QueryArgs().Add("page", "1")
	c.Request.URI().QueryArgs().Add("page_size", "10")

	h.List(context.Background(), c)

	assert.DeepEqual(t, consts.StatusOK, c.Response.StatusCode())
	resp := parseResp(t, c)
	assert.DeepEqual(t, 0, resp.Code)
}

func TestList_Empty(t *testing.T) {
	h, _ := newHandler()

	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)

	h.List(context.Background(), c)

	resp := parseResp(t, c)
	assert.DeepEqual(t, 0, resp.Code)
}

func TestList_ServiceError(t *testing.T) {
	h, svc := newHandler()
	svc.listErr = bizerrors.NewBizError(500, "db error")

	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)

	h.List(context.Background(), c)

	resp := parseResp(t, c)
	assert.DeepEqual(t, 500, resp.Code)
}

// --- Update tests ---

func TestUpdate_Success(t *testing.T) {
	h, svc := newHandler()
	_, _ = svc.Create(context.Background(), &CreateItemReq{Name: "old"})

	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPut)
	setPathParam(c, "id", "1")
	setJSONBody(c, `{"name":"new"}`)

	h.Update(context.Background(), c)

	assert.DeepEqual(t, consts.StatusOK, c.Response.StatusCode())
	resp := parseResp(t, c)

	var got Item
	_ = json.Unmarshal(resp.Data, &got)
	assert.DeepEqual(t, "new", got.Name)
}

func TestUpdate_InvalidID(t *testing.T) {
	h, _ := newHandler()
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPut)
	setPathParam(c, "id", "xyz")
	setJSONBody(c, `{"name":"new"}`)

	h.Update(context.Background(), c)

	assert.DeepEqual(t, consts.StatusBadRequest, c.Response.StatusCode())
}

func TestUpdate_NotFound(t *testing.T) {
	h, _ := newHandler()
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPut)
	setPathParam(c, "id", "999")
	setJSONBody(c, `{"name":"new"}`)

	h.Update(context.Background(), c)

	resp := parseResp(t, c)
	assert.DeepEqual(t, 404, resp.Code)
}

// --- Delete tests ---

func TestDelete_Success(t *testing.T) {
	h, svc := newHandler()
	_, _ = svc.Create(context.Background(), &CreateItemReq{Name: "doomed"})

	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodDelete)
	setPathParam(c, "id", "1")

	h.Delete(context.Background(), c)

	assert.DeepEqual(t, consts.StatusOK, c.Response.StatusCode())
	resp := parseResp(t, c)
	assert.DeepEqual(t, 0, resp.Code)
}

func TestDelete_InvalidID(t *testing.T) {
	h, _ := newHandler()
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodDelete)
	setPathParam(c, "id", "not-a-number")

	h.Delete(context.Background(), c)

	assert.DeepEqual(t, consts.StatusBadRequest, c.Response.StatusCode())
}

func TestDelete_NotFound(t *testing.T) {
	h, svc := newHandler()
	svc.deleteErr = bizerrors.NewBizError(404, "not found")

	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodDelete)
	setPathParam(c, "id", "999")

	h.Delete(context.Background(), c)

	resp := parseResp(t, c)
	assert.DeepEqual(t, 404, resp.Code)
}

// --- parseID tests ---

func TestParseID_Valid(t *testing.T) {
	c := app.NewContext(0)
	setPathParam(c, "id", "42")

	id, err := parseID(c)
	assert.Nil(t, err)
	assert.DeepEqual(t, int64(42), id)
}

func TestParseID_Invalid(t *testing.T) {
	c := app.NewContext(0)
	setPathParam(c, "id", "abc")

	_, err := parseID(c)
	if err == nil {
		t.Error("expected error for invalid id")
	}
}

func TestParseID_Empty(t *testing.T) {
	c := app.NewContext(0)

	_, err := parseID(c)
	if err == nil {
		t.Error("expected error for empty id")
	}
}

// --- RegisterRoutes tests ---

func TestRegisterRoutes(t *testing.T) {
	h, _ := newHandler()

	engine := route.NewEngine(config.NewOptions(nil))
	rg := engine.Group("/api")

	// Should not panic
	h.RegisterRoutes(rg, "/items")
}

func TestNewHandler_NotNil(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, CreateItemReq, UpdateItemReq](svc)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

// --- Sort validation tests ---

// TestList_SortInjectionRejected 回归测试：sort 参数曾直通 ORDER BY（SQL 注入），
// 现在必须经严格标识符校验，注入形态一律 400。
func TestList_SortInjectionRejected(t *testing.T) {
	h, _ := newHandler()

	payloads := []string{
		"(SELECT CASE WHEN (SELECT 1)=1 THEN id ELSE name END)",
		"id--",
		"' UNION SELECT password FROM users",
		"1=1",
		"id asc, (SELECT 1)",
		"id ascending", // 非法方向词
	}
	for _, payload := range payloads {
		c := app.NewContext(0)
		c.Request.SetMethod(consts.MethodGet)
		c.Request.URI().QueryArgs().Add("sort", payload)
		h.List(context.Background(), c)
		if c.Response.StatusCode() != consts.StatusBadRequest {
			t.Errorf("sort=%q: expected 400, got %d", payload, c.Response.StatusCode())
		}
	}
}

// TestList_SortValidFormats 合法排序格式放行（含多列、方向、大小写）。
func TestList_SortValidFormats(t *testing.T) {
	h, _ := newHandler()

	for _, s := range []string{"id", "name", "id desc", "name ASC", "id desc,name", "name, id asc"} {
		c := app.NewContext(0)
		c.Request.SetMethod(consts.MethodGet)
		c.Request.URI().QueryArgs().Add("sort", s)
		h.List(context.Background(), c)
		if c.Response.StatusCode() != consts.StatusOK {
			t.Errorf("sort=%q: expected 200, got %d", s, c.Response.StatusCode())
		}
	}
}

// TestList_SortAllowlist 白名单模式下，格式合法但不在白名单的列返回 400。
func TestList_SortAllowlist(t *testing.T) {
	svc := newMockItemService()
	h := NewHandler[Item, CreateItemReq, UpdateItemReq](svc,
		WithSortableColumns[Item, CreateItemReq, UpdateItemReq]("id", "name"))

	for _, s := range []string{"id desc", "NAME"} {
		c := app.NewContext(0)
		c.Request.SetMethod(consts.MethodGet)
		c.Request.URI().QueryArgs().Add("sort", s)
		h.List(context.Background(), c)
		if c.Response.StatusCode() != consts.StatusOK {
			t.Errorf("sort=%q in allowlist: expected 200, got %d", s, c.Response.StatusCode())
		}
	}

	for _, s := range []string{"password", "id, secret"} {
		c := app.NewContext(0)
		c.Request.SetMethod(consts.MethodGet)
		c.Request.URI().QueryArgs().Add("sort", s)
		h.List(context.Background(), c)
		if c.Response.StatusCode() != consts.StatusBadRequest {
			t.Errorf("sort=%q not in allowlist: expected 400, got %d", s, c.Response.StatusCode())
		}
	}
}
