package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// packageVars is every package variable this package may declare. Plan step
// 2.5 moved every setting out of it, into core.Options and server.Options, so
// that each engine and each server has settings of its own; a new variable
// here would be shared by all of them again. What is left is set by the
// linker when the binary is built (go build -ldflags "-X ...") and read after.
var packageVars = map[string]string{
	"Version": "the release's version, stamped by the linker and never assigned in code",
}

// TestConfigHoldsNothingMutable reads the package's own declarations, and
// fails on any package variable packageVars does not list, and on any entry
// whose variable is gone.
func TestConfigHoldsNothingMutable(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					declared[id.Name] = true
					if _, ok := packageVars[id.Name]; !ok {
						t.Errorf("%s: package variable %s would be a setting every engine and server shares; "+
							"make it a field of core.Options or server.Options", fset.Position(id.Pos()), id.Name)
					}
				}
			}
		}
	}
	for name := range packageVars {
		if !declared[name] {
			t.Errorf("packageVars lists %s, which is no longer declared; remove the entry", name)
		}
	}
}

// configPath is this package's import path.
const configPath = "github.com/brandopakel/keel/internal/config"

// TestNothingAssignsTheBuildIdentity: the variables this package keeps are
// the linker's to set. A file anywhere in the module that assigns one, or
// takes its address, as a flag binding would, turns it back into a setting.
// Each file's own name for this package is read from its imports, so an
// alias or a dot import does not hide a write.
func TestNothingAssignsTheBuildIdentity(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	// names are what a file calls this package; bare is whether a listed
	// variable can be written without a qualifier, inside the package or
	// through a dot import.
	written := func(expr ast.Expr, names map[string]bool, bare bool) bool {
		switch e := ast.Unparen(expr).(type) {
		case *ast.Ident:
			_, listed := packageVars[e.Name]
			return bare && listed
		case *ast.SelectorExpr:
			pkg, ok := e.X.(*ast.Ident)
			_, listed := packageVars[e.Sel.Name]
			return ok && names[pkg.Name] && listed
		}
		return false
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "dist" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil // not Go this module builds, such as a template
		}
		checked++
		bare := file.Name.Name == "config" && filepath.Dir(path) == filepath.Join(root, "internal", "config")
		names := map[string]bool{}
		for _, spec := range file.Imports {
			if p, err := strconv.Unquote(spec.Path.Value); err != nil || p != configPath {
				continue
			}
			switch {
			case spec.Name == nil:
				names["config"] = true
			case spec.Name.Name == ".":
				bare = true
			case spec.Name.Name != "_":
				names[spec.Name.Name] = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if written(lhs, names, bare) {
						t.Errorf("%s assigns the build identity", fset.Position(lhs.Pos()))
					}
				}
			case *ast.IncDecStmt:
				if written(n.X, names, bare) {
					t.Errorf("%s assigns the build identity", fset.Position(n.Pos()))
				}
			case *ast.UnaryExpr:
				if n.Op == token.AND && written(n.X, names, bare) {
					t.Errorf("%s takes the address of the build identity", fset.Position(n.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("checked %d Go files under %s; the walk did not find the module", checked, root)
	}
}
