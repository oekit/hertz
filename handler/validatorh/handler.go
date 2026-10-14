// Package validatorh 提供基于 Hertz 的请求参数绑定与验证。
//
// 将 HTTP 框架绑定从 pkg/validator 分离，保持 pkg 层框架无关。
// 底层验证逻辑（Validate/ValidationError）仍在 pkg/validator 中。
package validatorh

import (
	"fmt"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/yi-nology/ekit-core/pkg/validator"
)

// BindQuery 绑定 query 参数并验证。
func BindQuery(c *app.RequestContext, obj any) error {
	if err := c.Bind(obj); err != nil {
		return wrapBindError("query", err)
	}
	return validator.Validate(obj)
}

// BindJSON 绑定 JSON body 并验证。
// 使用 c.Bind（仅绑定）+ validator.Validate（统一验证），避免重复验证。
// 注意：hertz 泛型 c.Bind 只在 Content-Type 为 application/json 时解析
// body，否则静默跳过（零值对象 + nil error）。这里对"带 body 但声明了
// 其他 Content-Type"显式报错，避免漏头请求被当成空参数成功。
// body 判断用实际字节而非 Content-Length：chunked 请求（无 Content-Length，
// hertz 中为 -1）同样携带 body，按长度判断会漏防。
func BindJSON(c *app.RequestContext, obj any) error {
	if len(c.Request.Body()) > 0 &&
		!strings.Contains(string(c.ContentType()), "application/json") {
		return wrapBindError("json", fmt.Errorf(
			"request body 需 Content-Type: application/json（得到 %q）", string(c.ContentType())))
	}
	if err := c.Bind(obj); err != nil {
		return wrapBindError("json", err)
	}
	return validator.Validate(obj)
}

// BindAndValidate 绑定请求参数并验证。
// 自动根据 Content-Type 选择绑定方式（JSON/query/form）。
// 使用 c.Bind（仅绑定）+ validator.Validate（统一验证），避免重复验证。
func BindAndValidate(c *app.RequestContext, obj any) error {
	if err := c.Bind(obj); err != nil {
		return wrapBindError("request", err)
	}
	return validator.Validate(obj)
}

// BindPath 绑定路径参数并验证。
func BindPath(c *app.RequestContext, obj any) error {
	if err := c.Bind(obj); err != nil {
		return wrapBindError("path", err)
	}
	return validator.Validate(obj)
}

// BindHeader 绑定请求头参数并验证。
func BindHeader(c *app.RequestContext, obj any) error {
	if err := c.Bind(obj); err != nil {
		return wrapBindError("header", err)
	}
	return validator.Validate(obj)
}

// wrapBindError 将绑定错误包装为 ValidationError。
func wrapBindError(source string, err error) error {
	if err == nil {
		return nil
	}
	if validator.IsValidationError(err) {
		return err
	}
	return validator.NewFieldValidationError([]validator.FieldError{
		{
			Field:   source,
			Tag:     "bind",
			Message: fmt.Sprintf("%s bind failed: %s", source, strings.TrimSpace(err.Error())),
		},
	})
}
