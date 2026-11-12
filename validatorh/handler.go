// Package validatorh 提供基于 Hertz 的请求参数绑定与验证。
//
// 将 HTTP 框架绑定从 pkg/validator 分离，保持 pkg 层框架无关。
// 底层验证逻辑（Validate/ValidationError）在 github.com/oekit/kitx/validator（自 pkg v0.10.0 拆出）。
//
// # 绑定口径（#220）
//
// BindQuery / BindPath / BindHeader / BindJSON 是**单源绑定**：分别只绑定
// query / path / header / JSON body 一个来源（底层调 hertz 的
// RequestContext.BindQuery / BindPath / BindHeader / BindJSON，只认对应
// struct tag）。此前四个函数全部调全量 c.Bind（path+query+header+body
// 四源混绑），godoc 声称的单源语义不成立——按「只信 query」写 handler 的
// 调用方存在跨源字段注入面（JSON body 里的字段能盖进 query 绑定的结构体）。
//
// 跨源全量绑定请用 [BindAndValidate]（显式命名，语义即全量）。
package validatorh

import (
	"fmt"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/oekit/kitx/validator"
)

// BindQuery 只绑定 query 参数（query tag）并验证——不吸收 path/header/body。
func BindQuery(c *app.RequestContext, obj any) error {
	if err := c.BindQuery(obj); err != nil {
		return wrapBindError("query", err)
	}
	return validator.Validate(obj)
}

// BindJSON 只绑定 JSON body 并验证——不吸收 query/path/header。
//
// Content-Type 闸门与 hertz 绑定判据**逐字同一口径**（#232）：
// ToLower(FilterContentType(ct)) == "application/json"。此前用大小写敏感的
// 子串包含——APPLICATION/JSON 这类 RFC 9110 大小写不敏感的合法媒体类型被
// 误拒，而 application/json-patch+json / jsonx / json-seq 这些**含子串但
// hertz 不解析**的媒体类型被放行后 body 静默丢弃（零值对象 + nil error，
// 正是闸门立意要杜绝的「漏头请求被当成空参数成功」，写操作变成静默空操作）。
// 判据与上游一致后，hertz 能解析的（唯一闸门放行）、hertz 不能解析的
// （显式报错而非静默吞 body）两边对齐。
//
// body 判断用实际字节而非 Content-Length：chunked 请求（无 Content-Length，
// hertz 中为 -1）同样携带 body，按长度判断会漏防。
func BindJSON(c *app.RequestContext, obj any) error {
	if len(c.Request.Body()) > 0 && !isJSONContentType(c) {
		return wrapBindError("json", fmt.Errorf(
			"request body 需 Content-Type: application/json（得到 %q）", string(c.ContentType())))
	}
	if err := c.BindJSON(obj); err != nil {
		return wrapBindError("json", err)
	}
	return validator.Validate(obj)
}

// isJSONContentType 复刻 hertz 绑定层的 Content-Type 判据
// （binding/default.go：ToLower(FilterContentType(ct)) == MIMEApplicationJSON），
// 参数剥离 + 小写归一，保证闸门放行的请求 hertz 一定能解析（#232）。
func isJSONContentType(c *app.RequestContext) bool {
	return strings.ToLower(utils.FilterContentType(string(c.ContentType()))) == consts.MIMEApplicationJSON
}

// BindAndValidate 全量绑定请求参数并验证（path/query/header/body 按
// Content-Type 混绑，hertz c.Bind 原生语义）。
// 使用 c.Bind（仅绑定）+ validator.Validate（统一验证），避免重复验证。
func BindAndValidate(c *app.RequestContext, obj any) error {
	if err := c.Bind(obj); err != nil {
		return wrapBindError("request", err)
	}
	return validator.Validate(obj)
}

// BindPath 只绑定路径参数（path tag）并验证——不吸收 query/header/body。
func BindPath(c *app.RequestContext, obj any) error {
	if err := c.BindPath(obj); err != nil {
		return wrapBindError("path", err)
	}
	return validator.Validate(obj)
}

// BindHeader 只绑定请求头参数（header tag）并验证——不吸收 query/path/body。
func BindHeader(c *app.RequestContext, obj any) error {
	if err := c.BindHeader(obj); err != nil {
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
