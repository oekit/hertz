package render

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"reflect"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app/server/render"
	"github.com/cloudwego/hertz/pkg/protocol"
)

// csvContentType 是 RFC 4180 推荐的 CSV Content-Type。
const csvContentType = "text/csv; charset=utf-8"

// CSV 用 encoding/csv 标准库序列化表格数据。Rows 第一行通常是表头。
// 业务可直接给 `[][]string`;行数据是 `[]any` / struct 时用
// [NewCSVFromAny] 预转成 `[][]string`(struct 行按导出字段逐列展开,
// 见其说明,#290)。
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

// NewCSVFromAny 把 `[]any` 类型的行(每行是 struct、`[]any`、`[]string` 或
// 单值)转成 `[][]string`,自动 strconv 格式化基础类型。复杂类型(map 等)走
// fmt.Sprintf("%v", v) 兜底。
//
// 行形态:
//   - `[]string`:原样作为一行;
//   - `[]any`:逐元素转字符串;
//   - struct / *struct(#290):按**导出字段**逐列展开——gorm 查询结果
//     (如 []User)由此得到真正的多列表格,而不是整行 `%v` dump 成单列;
//     指针字段解引用(nil 按空串),未导出字段跳过。**不自动加表头**:
//     需要表头时自行 prepend 一行 `[]string`(字段名序与 struct 字段序
//     一致),行数不因此变化;
//   - 其他单值:作为单列。
//
// 典型用法(把 gorm 查询结果直接转 CSV):
//
//	rows := []any{user1, user2}
//	ctx.Render(200, hertzrender.NewCSVFromAny(append(
//	    []any{[]string{"name", "age"}}, rows...)))  // 表头自行 prepend
func NewCSVFromAny(rows []any) CSV {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		if s, ok := structRowToStrings(row); ok {
			out = append(out, s)
			continue
		}
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

// structRowToStrings 把 struct(或非 nil 的 *struct)行按导出字段展开成一行。
// 不是 struct 行(或没有任何可读导出字段,如 time.Time 这类全未导出结构)
// 时返回 ok=false,交回类型开关走单值 `%v` 兜底。
func structRowToStrings(row any) ([]string, bool) {
	rv := reflect.ValueOf(row)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false // nil 指针走单值分支(%v → "<nil>")
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	rt := rv.Type()
	out := make([]string, 0, rt.NumField())
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue // 未导出字段不可读,跳过
		}
		out = append(out, reflectFieldValue(rv.Field(i)))
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// reflectFieldValue 把字段值转字符串:指针逐层解引用(nil 按空串——gorm 的
// 可空列常是 *string/*int,直接 %v 会打成指针地址),其余走 [anyToString]。
func reflectFieldValue(fv reflect.Value) string {
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return ""
		}
		fv = fv.Elem()
	}
	if !fv.CanInterface() {
		return ""
	}
	return anyToString(fv.Interface())
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
