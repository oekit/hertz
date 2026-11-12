package crudh

import (
	"database/sql/driver"
	"reflect"
	"strings"
	"time"
)

// entityColumns 按实体类型 T 的字段反射生成排序列白名单（#165、#175）。
//
// 返回「别名 → 规范化列名」映射：规范化列名与 gorm 的 DB 真实列名对齐
// （gorm `column:` tag 优先；无 tag 字段套 gorm NamingStrategy 的蛇形转换，
// 见 [snakeColumn]）——修复前白名单收的是 Go 字段名小写（如 DisplayName →
// "displayname"），与 NamingStrategy 产出的真列名 "display_name" 脱节，
// 按别名校验通过后 "displayname" 原文进 ORDER BY ⇒ 列不存在 ⇒ DB 报错 5xx。
// 修复后别名只决定「能不能排」，出 SQL 恒用规范化列名。
//
// 别名集合 = 规范化列名本身 + json tag 名（小写）。Go 字段名小写**不再是**
// 别名（与真列名脱节的正是它；无 tag 字段的字段名小写 ≠ 蛇形真列名时旧行为
// 本就产生 5xx，有此需要的用 gorm column tag 或 json tag 显式声明）。
//
// 嵌入结构体（#175）：gorm 对嵌入类型只**提升内层字段**为列、不为嵌入类型
// 本身建列——白名单同样只收内层字段（gorm.Model 的 ID/CreatedAt 等真实列），
// 嵌入类型名（"model"）绝不作为列入表。嵌入的判定与 gorm 一致：匿名结构体
// （time.Time / 实现 driver.Valuer 的类型除外，gorm 对它们建单列）或显式
// `gorm:"embedded"`；`embeddedPrefix` 前缀拼进内层列名（嵌套嵌入累加前缀）。
//
// 实体不是结构体 / 无可用字段 ⇒ 空集（白名单外的列一律 400，fail-closed）。
func entityColumns[T any]() map[string]string {
	cols := make(map[string]string)
	collectEntityColumnsWithPrefix(reflect.TypeOf((*T)(nil)).Elem(), cols, map[reflect.Type]struct{}{}, "")
	return cols
}

// valuerType 是 driver.Valuer 的反射类型：匿名嵌入实现了 Valuer 的结构体时，
// gorm 不展开内层字段，而是把该字段当**单个**列（经 Value() 取值）——白名单
// 同样只收字段名对应的单列。
var valuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()

// collectEntityColumnsWithPrefix 递归收集结构体字段的「别名 → 规范化列名」，
// prefix 是外层嵌入链累积的 embeddedPrefix（gorm 语义：内层列名 = 前缀 +
// 内层列名，column tag 也被加前缀）。
//
// 嵌入判定对齐 gorm schema.ParseField（v1.31.2）：
//   - 显式 `gorm:"embedded"`（具名字段/匿名皆可）⇒ 嵌入，展开内层字段；
//   - 匿名结构体且非 time.Time、未实现 driver.Valuer ⇒ 嵌入，展开内层字段；
//   - 匿名 time.Time / Valuer / 非结构体 ⇒ 不嵌入，gorm 按字段名建**单列**
//     （如匿名 time.Time → 列 "time"），白名单同样只收该单列；
//   - 显式 embedded 的 time.Time（内层全非导出，展开后无列）/ 非结构体
//     （gorm 报 invalid embedded struct）⇒ 无列，跳过。
//
// seen 按类型防环；同一类型以多个不同前缀重复嵌入时只收首个前缀下的列
// （gorm 侧同类型多前缀嵌入会产生同名/重复列，本身不是合法 schema）。
func collectEntityColumnsWithPrefix(t reflect.Type, cols map[string]string, seen map[reflect.Type]struct{}, prefix string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	if _, ok := seen[t]; ok {
		return
	}
	seen[t] = struct{}{}
	timeType := reflect.TypeOf(time.Time{})
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			// 非导出字段：不是 DB 列，不进白名单。
			continue
		}
		gormTag := f.Tag.Get("gorm")
		if gormSkipField(gormTag) {
			// gorm:"-" / gorm:"-:all"：字段整体忽略（不可读写），不是 DB 列。
			continue
		}
		embeddedByTag := gormHasOption(gormTag, "embedded")
		if f.Anonymous || embeddedByTag {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != timeType &&
				(embeddedByTag || !reflect.PointerTo(ft).Implements(valuerType)) {
				// 嵌入结构体：gorm 只提升内层字段，类型本身不是列（#175）——
				// 递归收内层并 continue，嵌入类型名不再入表。
				collectEntityColumnsWithPrefix(ft, cols, seen, prefix+gormOptionValue(gormTag, "embeddedPrefix"))
				continue
			}
			if embeddedByTag {
				// 显式 embedded 的 time.Time / 非结构体：gorm 侧无可用列。
				continue
			}
			// 匿名且未声明 embedded 的 time.Time / Valuer / 非结构体：
			// gorm 按字段名建单列 → 落到下方通用单列收集。
		}
		// 规范化列名：gorm `column:` tag 优先；无 tag 套 NamingStrategy 蛇形
		// 转换（与 gorm 建表时的真列名对齐，#165）。
		col := prefix + snakeColumn(f.Name)
		if c := gormColumnTag(gormTag); c != "" {
			col = prefix + c
		}
		addColumn(cols, col, col)
		if tag, ok := f.Tag.Lookup("json"); ok {
			name := strings.Split(tag, ",")[0]
			if name != "" && name != "-" {
				// json 名是别名：放行校验，出 SQL 用规范化列名。
				addColumn(cols, name, col)
			}
		}
	}
}

// addColumn 把「别名 → 规范化列名」按小写口径加入白名单。
// 别名为空或规范化列名为空时不收（fail-closed，不产出空列名的 ORDER BY）。
func addColumn(cols map[string]string, alias, col string) {
	alias = strings.ToLower(alias)
	if alias == "" || col == "" {
		return
	}
	cols[alias] = col
}

// commonInitialisms 是 gorm schema.NamingStrategy 的缩写词表（逐字对齐，
// 顺序也一致——strings.NewReplacer 在同一位置命中多个 pattern 时按参数顺序
// 取第一个）。
var commonInitialisms = []string{"API", "ASCII", "CPU", "CSS", "DNS", "EOF", "GUID", "HTML", "HTTP", "HTTPS", "ID", "IP", "JSON", "LHS", "QPS", "RAM", "RHS", "RPC", "SLA", "SMTP", "SSH", "TLS", "TTL", "UID", "UI", "UUID", "URI", "URL", "UTF8", "VM", "XML", "XSRF", "XSS"}

// initialismsReplacer 把缩写词改写成「首字母大写 + 其余小写」的驼峰形态
// （gorm 用 golang.org/x/text 的 cases.Title，本仓零依赖口径用等价的手工
// 首字母大写——缩写词表全为 ASCII 大写，两者逐词等价）。
var initialismsReplacer = func() *strings.Replacer {
	pairs := make([]string, 0, len(commonInitialisms)*2)
	for _, in := range commonInitialisms {
		pairs = append(pairs, in, strings.ToUpper(in[:1])+strings.ToLower(in[1:]))
	}
	return strings.NewReplacer(pairs...)
}()

// snakeColumn 把 Go 字段名转成 gorm NamingStrategy 的蛇形列名（#165）。
//
// 算法逐字对齐 gorm schema.NamingStrategy.toDBName（v1.31.2，已用真实
// NamingStrategy 对缩写词表全量回归：ID→id、UserID→user_id、DisplayName→
// display_name、HTTPServer→http_server、OAuth2Token→o_auth2_token 等，
// 见 snake_column_test.go）——白名单产出的列名必须与 gorm 建表列名一致，
// 否则「校验通过的列」依然是 DB 里不存在的列（本修复要消灭的正是这个）。
func snakeColumn(name string) string {
	if name == "" {
		return ""
	}
	value := initialismsReplacer.Replace(name)

	var buf strings.Builder
	var lastCase, nextCase, nextNumber bool // upper case == true
	curCase := value[0] >= 'A' && value[0] <= 'Z'

	for i, v := range value[:len(value)-1] {
		nextCase = value[i+1] >= 'A' && value[i+1] <= 'Z'
		nextNumber = value[i+1] >= '0' && value[i+1] <= '9'

		if curCase {
			if lastCase && (nextCase || nextNumber) {
				buf.WriteRune(v + 32)
			} else {
				if i > 0 && value[i-1] != '_' && value[i+1] != '_' {
					buf.WriteByte('_')
				}
				buf.WriteRune(v + 32)
			}
		} else {
			buf.WriteRune(v)
		}

		lastCase = curCase
		curCase = nextCase
	}

	if curCase {
		if !lastCase && len(value) > 1 {
			buf.WriteByte('_')
		}
		buf.WriteByte(value[len(value)-1] + 32)
	} else {
		buf.WriteByte(value[len(value)-1])
	}
	return buf.String()
}

// gormColumnTag 从 gorm tag 中取 column 名（无则空串）。
// 形如 `gorm:"column:created_at;index"` → "created_at"。
func gormColumnTag(tag string) string {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, "column:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// gormHasOption 判断 gorm tag 是否声明了某选项（形如 `gorm:"embedded;embeddedPrefix:au_"`
// 中的 "embedded"）。与 gorm 的 TagSettings 口径一致：分号分隔，选项名不区分大小写，
// 带值/不带值都算声明（gorm 对 EMBEDDED 无论是否带值都视为声明）。
func gormHasOption(tag, option string) bool {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if key, _, found := strings.Cut(part, ":"); found {
			if strings.EqualFold(strings.TrimSpace(key), option) {
				return true
			}
			continue
		}
		if strings.EqualFold(part, option) {
			return true
		}
	}
	return false
}

// gormOptionValue 取 gorm tag 中某选项的值（无则空串）。
// 形如 `gorm:"embedded;embeddedPrefix:author_"` 中 embeddedPrefix → "author_"。
// 值内的 ":" 按 gorm ParseTagSetting 的口径保留（Join(values[1:], ":")）。
func gormOptionValue(tag, option string) string {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		key, rest, found := strings.Cut(part, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), option) {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// gormSkipField 判断 gorm tag 是否整体忽略字段：`gorm:"-"` / `gorm:"-:all"`
// 使字段不可读写（DataType 置空），gorm 不为其建列——白名单同样跳过；
// `gorm:"-:migration"` 只是不参与建表迁移，字段仍是可读写的真实列，不跳过。
// 值口径与 gorm 一致（小写 + 去空白后等于 "-" 或 "all"；裸 "-" 的值即 "-"，
// 见 ParseTagSetting 对无值选项 settings[k]=k 的处理）。
func gormSkipField(tag string) bool {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		key, val, found := strings.Cut(part, ":")
		if !strings.EqualFold(strings.TrimSpace(key), "-") {
			continue
		}
		if !found {
			val = "-"
		}
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "-", "all":
			return true
		}
		return false
	}
	return false
}

// sortAllowlist 返回当前生效的排序列白名单（别名 → 规范化列名）；nil 表示
// 不做白名单校验（全放开，仅标识符格式校验）。见 [WithSortValidation]。
func (h *Handler[T, ID, C, U]) sortAllowlist() map[string]string {
	if !h.sortValidation {
		return nil
	}
	return h.sortable
}
