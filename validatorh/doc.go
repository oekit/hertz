// Package validatorh 提供基于 Hertz 的请求参数绑定与验证。
//
// HTTP 框架绑定层（自 pkg/validator 分离的先例延续）。
// 底层验证逻辑在 github.com/oekit/kitx/validator（自 pkg v0.10.0 拆出）。
//
// 用法:
//
//	var req CreateReq
//	if err := validatorh.BindAndValidate(c, &req); err != nil {
//	    mw.Fail(ctx, c, err)
//	    return
//	}
package validatorh
