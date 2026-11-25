package pg_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hostryhq/go-auth/scopelint"
)

func goFiles(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				out = append(out, filepath.Join(d, n))
			}
		}
	}
	return out
}

// TC-05: the tenant context is set only by the go-common helper; no SET app.* and no set_config of the
// tenant outside it. The only other session setting is the invitation capability (app.invite_hash).
func TestTenantContextIsNeverSetOutsideTheHelper(t *testing.T) {
	for _, f := range goFiles(t, ".", "../../service", "../../api", "../mem") {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ToLower(string(b))
		if strings.Contains(src, "set app.") || strings.Contains(src, "set_config('app.tenant_id'") || strings.Contains(src, `set_config("app.tenant_id"`) {
			t.Errorf("%s sets the tenant context outside go-common's tenancy helper", f)
		}
		if strings.Contains(src, "set_config('app.") && !strings.Contains(src, "set_config('app.invite_hash'") {
			t.Errorf("%s sets an unknown app.* setting", f)
		}
	}
}

// TC-05: SQL is never built from data. Every statement passed to the database is a string literal, or a
// concatenation of literals and the *Cols column-list constants.
func TestSQLIsNeverBuiltFromData(t *testing.T) {
	fset := token.NewFileSet()
	var bad []string
	for _, f := range goFiles(t, ".") {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			switch sel.Sel.Name {
			case "ExecContext", "QueryContext", "QueryRowContext":
			default:
				return true
			}
			if !constantSQL(call.Args[1]) {
				bad = append(bad, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	if len(bad) > 0 {
		t.Fatalf("statements not built from literals:\n%s", strings.Join(bad, "\n"))
	}
}

func constantSQL(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.STRING
	case *ast.BinaryExpr:
		return x.Op == token.ADD && constantSQL(x.X) && constantSQL(x.Y)
	case *ast.Ident:
		return strings.HasSuffix(x.Name, "Cols")
	}
	return false
}

// ADR-0003 A1.4 item 5: every statement on an outlet-scoped table carries a bound outlet_id predicate or an
// explained //scopelint:internal marker.
func TestOutletScopeLint(t *testing.T) {
	b, err := os.ReadFile("../../scopelint.tables.json")
	if err != nil {
		t.Fatal(err)
	}
	tables, err := scopelint.LoadTables(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".", "../../service"} {
		findings, err := scopelint.CheckDir(dir, tables)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range findings {
			t.Errorf("%v", f)
		}
	}
}
