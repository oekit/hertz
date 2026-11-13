package hertztracer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBareOptionType 回归 #247：本包不得再导出裸 Option 类型——AGENTS.md
// 「函数选项模式 XxxOption(禁止裸 Option,多包 co-import 冲突)」。hertztracer
// 与 cloudwego/hertz 公共配置包(pkg/common/config 同样导出 Option)同属一个
// hertz 应用的常见组合,裸名一经导出,消费方 dot-import 两包即编译失败
// (名字占位与是否被引用无关)——因此也不得以「兼容别名」形式回潮。语法级判定
// (go/parser),非文本 grep:struct 字段名(如 ServerTracing.Option)与
// selector(serverconfig.Option)不误伤。
func TestNoBareOptionType(t *testing.T) {
	files, err := filepath.Glob("./*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if ts.Name.IsExported() && ts.Name.Name == "Option" {
					t.Errorf("%s: bare exported type Option re-introduced - use XxxOption (AGENTS.md, #247)", path)
				}
			}
		}
	}
	// 新名与别名共存。
	var _ TracerOption
}
