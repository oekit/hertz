package validatorh

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route/param"
	"github.com/oekit/kitx/validator"
)

type queryForm struct {
	Name string `query:"name" validate:"required"`
	Age  int    `query:"age" validate:"gte=0"`
}

type jsonForm struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

type headerForm struct {
	Token string `header:"X-Token" validate:"required"`
}

// --- BindQuery ---

func TestBindQuery_Success(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	c.Request.URI().QueryArgs().Add("name", "alice")
	c.Request.URI().QueryArgs().Add("age", "30")

	var form queryForm
	err := BindQuery(c, &form)
	assert.Nil(t, err)
	assert.DeepEqual(t, "alice", form.Name)
	assert.DeepEqual(t, 30, form.Age)
}

func TestBindQuery_ValidationFails_MissingRequired(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	c.Request.URI().QueryArgs().Add("age", "30")

	var form queryForm
	err := BindQuery(c, &form)
	assert.NotNil(t, err)
}

func TestBindQuery_ValidationFails_InvalidValue(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	c.Request.URI().QueryArgs().Add("name", "alice")
	c.Request.URI().QueryArgs().Add("age", "-5")

	var form queryForm
	err := BindQuery(c, &form)
	assert.NotNil(t, err)
}

// --- BindJSON ---

func setJSONBody(c *app.RequestContext, body string) {
	c.Request.Header.SetContentTypeBytes([]byte("application/json"))
	c.Request.SetBodyStream(bytes.NewReader([]byte(body)), len(body))
}

func TestBindJSON_Success(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{"name":"alice","email":"alice@test.com"}`)

	var form jsonForm
	err := BindJSON(c, &form)
	assert.Nil(t, err)
	assert.DeepEqual(t, "alice", form.Name)
	assert.DeepEqual(t, "alice@test.com", form.Email)
}

func TestBindJSON_InvalidJSON_WrappedAsValidationError(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{invalid json`)

	var form jsonForm
	err := BindJSON(c, &form)
	assert.NotNil(t, err)
}

func TestBindJSON_ValidationFails_BadEmail(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{"name":"alice","email":"not-an-email"}`)

	var form jsonForm
	err := BindJSON(c, &form)
	assert.NotNil(t, err)
}

// --- BindAndValidate ---

func TestBindAndValidate_Success(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{"name":"bob","email":"bob@test.com"}`)

	var form jsonForm
	err := BindAndValidate(c, &form)
	assert.Nil(t, err)
	assert.DeepEqual(t, "bob", form.Name)
}

func TestBindAndValidate_ValidationFails_MissingRequired(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	setJSONBody(c, `{"email":"bob@test.com"}`)

	var form jsonForm
	err := BindAndValidate(c, &form)
	assert.NotNil(t, err)
}

// --- BindPath ---

func TestBindPath_ValidationFails_EmptyPath(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)

	var form struct {
		ID string `path:"id" validate:"required"`
	}
	err := BindPath(c, &form)
	assert.NotNil(t, err)
}

// --- BindHeader ---

func TestBindHeader_Success(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	c.Request.Header.Set("X-Token", "abc123")

	var form headerForm
	err := BindHeader(c, &form)
	assert.Nil(t, err)
	assert.DeepEqual(t, "abc123", form.Token)
}

func TestBindHeader_ValidationFails_MissingToken(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)

	var form headerForm
	err := BindHeader(c, &form)
	assert.NotNil(t, err)
}

// --- wrapBindError (tested indirectly) ---

func TestWrapBindError_NilError(t *testing.T) {
	err := wrapBindError("test", nil)
	assert.Nil(t, err)
}

func TestWrapBindError_PassesThroughValidationError(t *testing.T) {
	origErr := validator.NewFieldValidationError([]validator.FieldError{
		{Field: "name", Tag: "required", Message: "name is required"},
	})
	err := wrapBindError("test", origErr)
	if err != origErr {
		t.Error("ValidationError should pass through unchanged")
	}
}

func TestWrapBindError_WrapsGenericError(t *testing.T) {
	err := wrapBindError("json", errors.New("some bind error"))
	assert.NotNil(t, err)
	if !validator.IsValidationError(err) {
		t.Error("wrapped error should be a ValidationError")
	}
}

// TestBindJSON_NonJSONContentType_Rejected 回归：hertz 泛型 c.Bind 只在
// Content-Type 为 application/json 时解析 body，否则静默跳过（零值对象 +
// nil error）。带 body 但声明其他 Content-Type 的请求必须显式报错，
// 不能被当成空参数成功。
func TestBindJSON_NonJSONContentType_Rejected(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	c.Request.Header.SetContentTypeBytes([]byte("application/x-www-form-urlencoded"))
	body := "name=alice&email=alice@test.com"
	c.Request.SetBodyStream(bytes.NewReader([]byte(body)), len(body))

	var form jsonForm
	err := BindJSON(c, &form)
	if err == nil {
		t.Fatal("expected error for non-JSON Content-Type with body, got nil")
	}
	if !strings.Contains(err.Error(), "application/json") {
		t.Errorf("error should mention required Content-Type, got %v", err)
	}
}

// TestBindJSON_EmptyBody_NoContentType_OK：无 body 的请求（ContentLength<=0）
// 不受 Content-Type 校验影响，维持"空参数"语义。
func TestBindJSON_EmptyBody_NoContentType_OK(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	// 不设置 body、不设置 Content-Type

	var form jsonForm
	if err := BindJSON(c, &form); err == nil {
		t.Error("expected validation error (required fields empty), got nil")
	}
}

// ==================== 单源绑定（#220） ====================

// TestBindQuery_DoesNotAbsorbBody 回归 #220：BindQuery 只信 query——JSON body
// 里的同名字段不得混入（此前 c.Bind 全量绑定，body 字段被吸收进 query 结构体，
// 按 godoc「只信 query」写 handler 存在跨源注入面）。
func TestBindQuery_DoesNotAbsorbBody(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	c.Request.URI().QueryArgs().Add("name", "alice")
	c.Request.URI().QueryArgs().Add("age", "30")
	setJSONBody(c, `{"name":"evil","age":-1}`)

	var form queryForm
	if err := BindQuery(c, &form); err != nil {
		t.Fatalf("BindQuery: %v", err)
	}
	assert.DeepEqual(t, "alice", form.Name)
	assert.DeepEqual(t, 30, form.Age)
}

// TestBindJSON_DoesNotAbsorbQuery 回归 #220：BindJSON 只信 body——query 里的
// 同名字段不得混入。钉子：body 不带 name、query 带 name=attacker 时，
// required 校验必须失败（Name 保持零值），而不是被 query 偷偷填上。
func TestBindJSON_DoesNotAbsorbQuery(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodPost)
	c.Request.URI().QueryArgs().Add("name", "attacker")
	setJSONBody(c, `{"email":"alice@test.com"}`)

	var form jsonForm
	err := BindJSON(c, &form)
	if err == nil {
		t.Fatal("query params must not leak into BindJSON target (required name stays empty)")
	}
	assert.DeepEqual(t, "", form.Name)
}

// TestBindPath_OnlyPathSource 回归 #220：BindPath 只信 path tag——query 同名
// 参数不得混入。
func TestBindPath_OnlyPathSource(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	c.Request.URI().QueryArgs().Add("id", "999")
	c.Params = param.Params{{Key: "id", Value: "42"}}

	var form struct {
		ID string `path:"id" validate:"required"`
	}
	if err := BindPath(c, &form); err != nil {
		t.Fatalf("BindPath: %v", err)
	}
	assert.DeepEqual(t, "42", form.ID)
}

// TestBindHeader_OnlyHeaderSource 回归 #220：BindHeader 只信 header tag——
// query 同名参数不得混入。
func TestBindHeader_OnlyHeaderSource(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(consts.MethodGet)
	c.Request.URI().QueryArgs().Add("token", "evil")
	c.Request.Header.Set("X-Token", "abc123")

	var form headerForm
	if err := BindHeader(c, &form); err != nil {
		t.Fatalf("BindHeader: %v", err)
	}
	assert.DeepEqual(t, "abc123", form.Token)
}

// ==================== Content-Type 闸门口径（#232） ====================

// TestBindJSON_ContentTypeMatrix 回归 #232：闸门必须与 hertz 绑定判据同一口径
// （ToLower(FilterContentType(ct)) == "application/json"）——
//   - RFC 9110 大小写不敏感的合法媒体类型（APPLICATION/JSON）放行（此前误拒 4xx）；
//   - 含子串但 hertz 不解析的媒体类型（json-patch+json / jsonx / json-seq）
//     显式报错（此前放行后 body 被静默丢弃，写操作变成静默空操作）。
func TestBindJSON_ContentTypeMatrix(t *testing.T) {
	tests := []struct {
		ct     string
		wantOK bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"APPLICATION/JSON", true},
		{"Application/Json", true},
		{"application/json-patch+json", false},
		{"application/jsonx", false},
		{"application/json-seq", false},
		{"application/x-www-form-urlencoded", false},
	}
	for _, tt := range tests {
		t.Run(tt.ct, func(t *testing.T) {
			c := app.NewContext(0)
			c.Request.SetMethod(consts.MethodPost)
			c.Request.Header.SetContentTypeBytes([]byte(tt.ct))
			setRawBody(c, `{"name":"alice","email":"alice@test.com"}`)

			var form jsonForm
			err := BindJSON(c, &form)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("BindJSON with CT %q: %v (hertz parses this media type, gate must not reject)", tt.ct, err)
				}
				assert.DeepEqual(t, "alice", form.Name)
				assert.DeepEqual(t, "alice@test.com", form.Email)
				return
			}
			if err == nil {
				t.Fatalf("BindJSON with CT %q: nil error (body would be silently dropped, #232)", tt.ct)
			}
			assert.DeepEqual(t, "", form.Name)
			if !strings.Contains(err.Error(), "Content-Type") {
				t.Errorf("error %v must mention Content-Type", err)
			}
		})
	}
}

// setRawBody 与 setJSONBody 相同，但 Content-Type 由调用方设定（矩阵用）。
func setRawBody(c *app.RequestContext, body string) {
	c.Request.SetBodyStream(bytes.NewReader([]byte(body)), len(body))
}
