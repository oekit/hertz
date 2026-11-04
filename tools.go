//go:build tools

// 构建图钉版（#109）：把 genproto 显式 require 钉在 module graph 里。
//
// 背景：旧单体 google.golang.org/genproto（2019-08，经 tjfoc/gmsm → grpc v1.31.0
// 的 go.mod 图边入选）仍含 googleapis/rpc/status，与分裂模块
// genproto/googleapis/rpc 撞包——go1.27 工具链的包装载判定下 nns 包 setup
// 阶段 ambiguous import。主 go.mod 显式 require 新单体（googleapis/rpc 已拆走）
// 后歧义消失，但**没有任何 import 边指向新单体时，go mod tidy 会把这条
// require 剪掉**（CI 的 tidy -diff 门禁即红）。本文件以 //go:build tools 标签
// 提供一条「只被 tidy 看见、不被正常编译」的 import 边，require 从此稳住：
//
//   - 正常构建（无 tools tag）：本文件不参与编译，零开销；
//   - go mod tidy：本文件的 import 计入图，genproto require 保留；
//
// 所选包 protobuf/field_mask 是新单体的纯别名占位包，只 import 已在依赖图内的
// google.golang.org/protobuf——不引入任何新第三方依赖。
package hertz

import _ "google.golang.org/genproto/protobuf/field_mask"
