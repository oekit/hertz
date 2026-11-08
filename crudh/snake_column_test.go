package crudh

import "testing"

// TestSnakeColumn_GormNamingStrategyGolden 是 #165 的算法锚点：snakeColumn
// 必须与 gorm schema.NamingStrategy.toDBName 逐例一致——白名单产出的列名
// 就是 gorm 建表的真列名，错一个字母就是「校验通过的列在 DB 里不存在」。
//
// 期望值用 gorm.io/gorm v1.31.2 的真实 NamingStrategy.ColumnName("", name)
// 录制（2026-11-08 离线脚本跑出），覆盖：普通驼峰、连续大写缩写词（ID/HTTP/
// UTF8 等全表）、缩写词与普通词混合、数字边界（OAuth2/SHA256/UID2）、单字母、
// 首字母小写。gorm 侧算法变更时本表即失败锚点（白名单口径必须跟着 gorm 走）。
func TestSnakeColumn_GormNamingStrategyGolden(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain lower", "name", "name"},
		{"plain camel", "DisplayName", "display_name"},
		{"createdAt", "CreatedAt", "created_at"},
		{"single upper", "A", "a"},
		{"single word upper", "HTML", "html"},
		{"id initialism", "ID", "id"},
		{"ids plural", "IDs", "ids"},
		{"user id", "UserID", "user_id"},
		{"api key", "APIKey", "api_key"},
		{"http server", "HTTPServer", "http_server"},
		{"url path", "URLPath", "url_path"},
		{"utf8 body", "UTF8Body", "utf8_body"},
		{"oauth2 token", "OAuth2Token", "o_auth2_token"},
		{"sha256 sum", "SHA256Sum", "sha256_sum"},
		{"vm size", "VmSize", "vm_size"},
		{"xsrf token", "XSRFToken", "xsrf_token"},
		{"sn two caps", "SN", "sn"},
		{"ttl seconds", "TTLSeconds", "ttl_seconds"},
		{"ip", "IP", "ip"},
		{"uid2", "UID2", "uid2"},
		{"lower first hump", "aB", "a_b"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := snakeColumn(tt.in); got != tt.want {
				t.Errorf("snakeColumn(%q) = %q, want %q (gorm v1.31.2 NamingStrategy golden)", tt.in, got, tt.want)
			}
		})
	}
}

// TestEntityColumns_InitialismFieldsUseSnakeCanonical 缩写词字段的白名单
// 产出必须是蛇形真列名（#165）：修复前 DisplayName 进白名单的是 Go 字段名
// 小写 "userid"，与 gorm 建列 "user_id" 脱节——正是 issue 描述的 5xx 形态。
func TestEntityColumns_InitialismFieldsUseSnakeCanonical(t *testing.T) {
	type entity struct {
		ID      int64  `json:"id"`
		UserID  int64  `json:"userId"`
		APIPath string `json:"apiPath"`
	}
	cols := entityColumns[entity]()

	want := map[string]string{
		"id":       "id",
		"user_id":  "user_id", // 蛇形真列名
		"userid":   "user_id", // json 名别名 → 真列名（修复前映射到 "userid" 自身 ⇒ ORDER BY userid ⇒ 5xx）
		"api_path": "api_path",
		"apipath":  "api_path", // json 名别名 → 蛇形真列名
	}
	for alias, canon := range want {
		if got := cols[alias]; got != canon {
			t.Errorf("alias %q maps to %q, want %q (set: %v)", alias, got, canon, cols)
		}
	}
	if len(cols) != len(want) {
		t.Errorf("unexpected extra aliases in %v (want exactly %v)", cols, want)
	}
}
