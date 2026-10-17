// Package validatorh 提供基于 Hertz 的请求参数绑定与验证。
//
// 将 HTTP 框架绑定逻辑从 pkg/validator 分离，保持 pkg 层框架无关。
// 底层验证逻辑（Validate/ValidationError）仍在 pkg/validator 中。
//
// 用法:
//
//	var req CreateReq
//	if err := validatorh.BindAndValidate(c, &req); err != nil {
//	    mw.Fail(ctx, c, err)
//	    return
//	}
package validatorh
