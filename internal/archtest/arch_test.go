// Package archtest 是仓库的结构守卫测试，不被任何包导入。
//
// 它把几件只能靠人记的约定变成会失败的测试：公开面里不出现上游模块的类型。
package archtest

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// publicPackages 是可以被外部导入的包目录。
var publicPackages = []string{
	"dialect", "relay", "channel", "plugin", "config", "server",
}

// upstreamPrefixes 是上游模块的类型前缀。公开面出现它们就等于把上游的类型
// 词汇变成本模块的兼容承诺。
var upstreamPrefixes = []string{
	"types.", "dto.", "relayconvert.", "convmeta.", "reasoning.", "kitutil.",
}

// allowedUpstreamUses 是明确接受的例外，键是「包名.符号名」。每一条都要有理由，
// 理由写在 docs/relay.md 的「与 RelayKit 的关系」里。
var allowedUpstreamUses = map[string]string{
	// 用量的字段结构由上游定义，镜像它只会带来漂移。
	"relay.PreparedResponse.Usage": "用量结构由 RelayKit 定义",
	"relay.Stream.Usage":           "用量结构由 RelayKit 定义",
}

func TestPublicSurfaceHasNoUpstreamTypes(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("解析仓库根目录失败: %v", err)
	}
	fileSet := token.NewFileSet()
	var problems []string
	seenAllowlist := map[string]bool{}

	for _, pkg := range publicPackages {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", pkg, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				t.Fatalf("解析 %s 失败: %v", path, err)
			}
			problems = append(problems, checkFile(file, fileSet, pkg, seenAllowlist)...)
		}
	}

	for key := range allowedUpstreamUses {
		if !seenAllowlist[key] {
			problems = append(problems,
				key+": 例外清单里的这一条已经用不上了，请从 arch_test.go 删掉")
		}
	}
	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	t.Fatalf("公开面出现上游类型 %d 处:\n  - %s", len(problems), strings.Join(problems, "\n  - "))
}

// checkFile 检查一个文件里全部导出声明的类型表达式。
//
// 只看导出部分：未导出的字段与方法不是公开面的一部分，结构体里混着上游的内部
// 状态是正常的。字段按「类型名.字段名」单独登记，这样同一个结构体里将来新增的
// 字段不会被旧的例外顺带放过。
func checkFile(file *ast.File, fileSet *token.FileSet, pkg string, seen map[string]bool) []string {
	var problems []string
	report := func(symbol string, node ast.Node) {
		text := render(fileSet, node)
		for _, prefix := range upstreamPrefixes {
			if strings.Contains(text, prefix) {
				problems = append(problems, pkg+"."+symbol+": "+text)
				return
			}
		}
	}
	exempt := func(symbol string) bool {
		key := pkg + "." + symbol
		if _, ok := allowedUpstreamUses[key]; ok {
			seen[key] = true
			return true
		}
		return false
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.FuncDecl:
			if !typed.Name.IsExported() {
				return true
			}
			symbol := typed.Name.Name
			if typed.Recv != nil && len(typed.Recv.List) > 0 {
				symbol = receiverName(typed.Recv.List[0].Type) + "." + symbol
			}
			if exempt(symbol) {
				return true
			}
			report(symbol, typed.Type)
		case *ast.TypeSpec:
			if !typed.Name.IsExported() {
				return true
			}
			if structType, ok := typed.Type.(*ast.StructType); ok {
				for _, field := range structType.Fields.List {
					for _, ident := range field.Names {
						if !ident.IsExported() {
							continue
						}
						symbol := typed.Name.Name + "." + ident.Name
						if exempt(symbol) {
							continue
						}
						report(symbol, field.Type)
					}
				}
				return true
			}
			if exempt(typed.Name.Name) {
				return true
			}
			report(typed.Name.Name, typed.Type)
		case *ast.ValueSpec:
			if typed.Type == nil {
				// 没有显式类型的常量不构成类型泄漏。
				return true
			}
			for _, ident := range typed.Names {
				if !ident.IsExported() || exempt(ident.Name) {
					continue
				}
				report(ident.Name, typed.Type)
			}
		}
		return true
	})
	return problems
}

// receiverName 取接收者的类型名。
func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return receiverName(typed.X)
	default:
		return "?"
	}
}

// render 把类型表达式打回源码形式。
func render(fileSet *token.FileSet, node ast.Node) string {
	var builder strings.Builder
	if err := printer.Fprint(&builder, fileSet, node); err != nil {
		return "<无法渲染>"
	}
	return builder.String()
}
