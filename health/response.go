package health

// Status 是健康检查的状态取值（三态）。
//
// 取值只有三种，见下方常量：
//
//   - StatusOK: 全部检查通过
//   - StatusDegraded: 软依赖降级（Checker 返回 health.Degraded），HTTP 仍 200，就绪不失败
//   - StatusError: 硬失败，HTTP 503
//
// 消费方比较状态请用这三个常量，不要硬编码字面量——
// 裸字符串比较一旦拼错（如 "degrade"）是静默失效，恒不命中。
type Status string

const (
	// StatusOK 全部检查通过。
	StatusOK Status = "ok"
	// StatusDegraded 存在软依赖降级项：HTTP 仍 200，但需告警识别（见 health.Degraded）。
	StatusDegraded Status = "degraded"
	// StatusError 存在硬失败项：HTTP 503，就绪探针失败。
	StatusError Status = "error"
)

// HealthResponse 是 /health 端点的完整响应体，可被消费方直接解码，
// 不必再手写一份 JSON 信封结构体去猜内层形态：
//
//	var resp health.HealthResponse
//	if err := json.Unmarshal(body, &resp); err != nil {
//	    return err
//	}
//	if resp.Data.Status == health.StatusDegraded {
//	    // 软依赖降级：就绪不失败，但要告警
//	}
//	for _, ck := range resp.Data.Checks {
//	    if ck.Status == health.StatusError {
//	        log.Printf("check %s failed: %s", ck.Name, ck.Error)
//	    }
//	}
//
// 实际响应体形如（健康检查通过时）：
//
//	{"code":0,"data":{"checks":[{"name":"db","status":"ok"}],"status":"ok"},"message":"success","trace_id":""}
//
// # 消费方极易猜错的 4 点
//
//  1. status 与 checks 都在 data 下，不在顶层。顶层只有 code/data/message/trace_id。
//     按「顶层 status」解析不会报错，而是取到空串——健康状态整个静默丢失。
//
//  2. checks 是数组 [{name,status,error}]，不是 name → {...} 的 map。
//     按 map 解析会整个反序列化失败，逐项失败项全部丢失。
//
//  3. 单项详情在 error 字段，不是 message。message 是顶层信封消息
//     （"success" / "service degraded" / "service unhealthy"），
//     与「是哪一项坏了」无关；单项错误详情看 checks[i].error。
//
//  4. status 取值三态：ok / degraded / error，见 Status 常量。
//     比较请用常量，不要硬编码字面量——写错（如 "degrade"）是静默失效，恒不命中。
//
// # 适用范围
//
// HealthResponse 只描述 /health。/ping 的 data 为 null、/buildVersion 的 data 是
// 版本字段（version/buildCommit/buildTime/buildBranch/startTime/uptime），
// 两者请按各自形状解析。
type HealthResponse struct {
	// Code 业务状态码：0=成功（ok 或 degraded），503=失败（error）。
	Code int `json:"code"`
	// Data 健康检查载荷：整体状态 + 逐项明细。
	Data HealthData `json:"data"`
	// Message 信封描述信息：success / service degraded / service unhealthy。
	Message string `json:"message"`
	// TraceID 链路追踪 ID，贯穿请求日志，用于问题排查。
	TraceID string `json:"trace_id"`
}

// HealthData 是 HealthResponse 的 data 部分：整体状态 + 逐项检查结果。
//
// 注意 status 与 checks 都在 data 内，不在信封顶层（见 HealthResponse 的说明）。
//
// 字段顺序（checks 在前、status 在后）与既有响应体的字节序保持一致，
// 避免消费方按字节比对响应时出现无谓 diff。
type HealthData struct {
	// Checks 逐项检查结果，按 checker 注册顺序稳定输出。
	// 是数组（不是 map），元素为 {name,status,error}，见 CheckResult。
	Checks []CheckResult `json:"checks"`
	// Status 整体状态：ok / degraded / error，见 Status 常量。
	// 汇总规则：任一单项 error → error；否则任一单项 degraded → degraded；否则 ok。
	Status Status `json:"status"`
}
