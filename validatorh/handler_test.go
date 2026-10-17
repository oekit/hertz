package validatorh

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
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
