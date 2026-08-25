package gate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProductionGateExportsOnlyConfiguredConstructor(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	packages, err := parser.ParseDir(token.NewFileSet(), filepath.Dir(file), func(info os.FileInfo) bool {
		return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range packages["gate"].Files {
		for _, declaration := range source.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if ok && fn.Recv == nil && (fn.Name.Name == "NewHandler" || fn.Name.Name == "NewHandlerWithOptions") {
				t.Fatalf("legacy production constructor remains exported: %s", fn.Name.Name)
			}
		}
	}
}
