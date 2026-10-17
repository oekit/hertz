package render

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app/server/render"
	"github.com/cloudwego/hertz/pkg/protocol"
)

// csvContentType 是 RFC 4180 推荐的 CSV Content-Type。
const csvContentType = "text/csv; charset=utf-8"

// CSV 用 encoding/csv 标准库序列化表格数据。Rows 第一行通常是表头。
// 业务可直接给 `[][]string`;若想给 `[]any` / `[]struct`,在 NewCSV 时
// 调 convertFields() 预转。
//
// 用法:
//
//	ctx.Render(200, hertzrender.CSV{Rows: [][]string{
//	    {"name", "age"},
//	    {"alice", "30"},
//	}})
//
// 官方依据:https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/render/
// 自定义渲染段,encoding/csv 标准库文档 https://pkg.go.dev/encoding/csv。
type CSV struct {
	Rows [][]string
}

// Render 实现 render.Render interface。
func (r CSV) Render(resp *protocol.Response) error {
	r.WriteContentType(resp)
	if len(r.Rows) == 0 {
		// 空 CSV:写一个空行让客户端知道是合法 CSV
		resp.AppendBodyString("\n")
		return nil
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(r.Rows); err != nil {
		return fmt.Errorf("hertzrender: CSV write: %w", err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("hertzrender: CSV flush: %w", err)
	}
	resp.AppendBody(buf.Bytes())
	return nil
}

// WriteContentType 实现 render.Render interface。
func (r CSV) WriteContentType(resp *protocol.Response) {
	resp.Header.SetContentType(csvContentType)
}

// NewCSVFromAny 把 `[]any` 类型的行(每行是 struct 或 `[]any` 字段)转成
// `[][]string`,自动 strconv 格式化基础类型。复杂类型(struct/map)走
// fmt.Sprintf("%v", v) 兜底。
//
// 典型用法(把 gorm 查询结果直接转 CSV):
//
//	rows := []any{user1, user2}
//	ctx.Render(200, hertzrender.NewCSVFromAny(rows))
func NewCSVFromAny(rows []any) CSV {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		switch v := row.(type) {
		case []string:
			out = append(out, v)
		case []any:
			out = append(out, anyRowToStrings(v))
		default:
			// 单值:作为单列
			out = append(out, []string{anyToString(v)})
		}
	}
	return CSV{Rows: out}
}

func anyRowToStrings(row []any) []string {
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = anyToString(v)
	}
	return out
}

func anyToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint:
		return strconv.FormatUint(uint64(x), 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []byte:
		return string(x)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

// 确保 CSV 满足 render.Render(编译期检查)。
var _ render.Render = CSV{}

// ⚠️ CSV 公式注入(CSV Injection)警示:
// 本包不对单元格值做公式消毒 — 以 =、+、-、@ 开头的字符串(如用户可控的
// "=HYPERLINK(...)" / "@SUM(...)")写入 CSV 后,在 Excel/WPS 打开会作为
// 公式执行。把 gorm 查询结果/用户输入直接转 CSV 是高危路径,业务必须在
// 调用侧对不可信字段做前缀消毒(常见做法:前置单引号或空格)。
// 参考:OWASP "CSV Injection"。
