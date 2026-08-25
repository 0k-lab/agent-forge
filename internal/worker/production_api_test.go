package worker

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

func TestProductionWorkerExportsNoPathBearingRunner(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	packages, err := parser.ParseDir(token.NewFileSet(), filepath.Dir(file), func(info os.FileInfo) bool {
		return strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range packages["worker"].Files {
		for _, declaration := range source.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if ok && fn.Recv == nil && (fn.Name.Name == "Run" || fn.Name.Name == "RunWithOptions") {
				t.Fatalf("legacy path-bearing production runner remains exported: %s", fn.Name.Name)
			}
		}
	}
}
