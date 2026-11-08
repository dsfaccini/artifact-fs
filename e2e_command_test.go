//go:build !windows

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"testing"
)

// commandIn returns a command that runs name in dir, changing to dir after
// exec. Never set exec.Cmd.Dir in this package. On Linux the child changes to
// Cmd.Dir before exec, while it shares this process's memory and the thread
// that started it cannot stop. When Cmd.Dir is on a mount this process serves,
// a stop-the-world in that window waits forever on that thread, and the
// stopped FUSE server never answers the child.
func commandIn(dir, name string, args ...string) *exec.Cmd {
	if dir == "" {
		return exec.Command(name, args...)
	}
	return exec.Command("/bin/sh", append([]string{"-c", `cd "$0" && exec "$@"`, dir, name}, args...)...)
}

func TestNoCommandSetsDir(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var dir ast.Node
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Dir" {
						dir = sel
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Dir" {
					dir = key
				}
			}
			if dir != nil {
				t.Errorf("%s sets a Dir field; start the command with commandIn", fset.Position(dir.Pos()))
			}
			return true
		})
	}
}
