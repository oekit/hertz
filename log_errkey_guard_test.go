package hertz

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logxLogMethods 是 logx.Logger 的 kv 结构化日志方法集合。红线只管 kv 键位：
// printf 风格（*f 后缀）本就被 AGENTS.md 排除在主要手段之外、span tag
// （opentracing 标准 "error" tag）与域枚举值（health.Status = "error"）是
// 各自协议的合法字面量，不在本护栏范围。
var logxLogMethods = map[string]bool{
	"Trace": true, "Debug": true, "Info": true, "Notice": true,
	"Warn": true, "Error": true, "Fatal": true,
	"CtxTrace": true, "CtxDebug": true, "CtxInfo": true, "CtxNotice": true,
	"CtxWarn": true, "CtxError": true, "CtxFatal": true,
}

// TestLogErrKeyGuard 钉住 AGENTS.md 日志红线（#231）：logx kv 日志调用的
// 键位不得手写 "err"/"error" 字符串字面量——错误字段名统一用 logx.ErrKey
// 常量（值 "err"，两矩阵落同一 Loki/ES）。字面量一旦漂移，本仓就与按
// logx.ErrKey 做字段解析/静态扫描的下游整体脱钩（值班查不到关键错误）。
// 用 go/parser 做语法级判定（不是文本 grep）：只盯日志方法调用的 kv 键位，
// 注释、常量初始化、span tag、键位之外的 value 位都不误伤。
func TestLogErrKeyGuard(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != "." && (d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !logxLogMethods[sel.Sel.Name] {
				return true
			}
			args := call.Args
			// Ctx* 方法首个参数是 ctx；kv 键位从 msg 之后隔一个取一个。
			start := 0
			if strings.HasPrefix(sel.Sel.Name, "Ctx") {
				start = 1
			}
			if start >= len(args) {
				return true
			}
			// args[start] 是 msg，其后 (key, value) 交替——键位是偶偏移。
			for i := start + 1; i < len(args); i += 2 {
				lit, ok := args[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if lit.Value == `"err"` || lit.Value == `"error"` {
					pos := fset.Position(lit.Pos())
					t.Errorf("%s:%d: hand-written %s log kv key - use logx.ErrKey (AGENTS.md 硬红线, #231)",
						pos.Filename, pos.Line, lit.Value)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
