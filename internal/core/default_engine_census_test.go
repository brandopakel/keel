package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// defaultEngineUsers is every function and method, in this package's tests,
// in internal/server and in cmd/keel, that uses the default engine directly:
// that names defaultEngine or data_structure.DefaultSpace, or uses a package
// function over either (EvalAndResponse, ResetStores, OpenAOF, Configure and
// the rest, and data_structure's TotalKeys, Evicted and the rest). Each entry
// says which part of plan step 2.7 (docs/embedding-plan.md, "Step 2.7: the
// default engine") moves it onto an engine of its own; a part removes the
// entries it moves. Part 3 left none, and part 4 deletes the default engine,
// and this census with it.
//
// A caller of a listed helper is not listed: removing the helper breaks it.
// The package's own functions over the default engine are not listed either:
// they are the default engine, and part 4 deletes them.
var defaultEngineUsers = map[string]string{}

// TestDefaultEngineUsersAreCensused finds every direct use of the default
// engine outside the package's own functions over it, and fails on one that
// defaultEngineUsers does not list, and on a listed one that is gone. So a use
// added while step 2.7 is under way fails here, and so does an entry its part
// forgot to remove.
//
// It reads syntax, not types, as the parallel census does: in this package's
// own tests a package function is one of its names used bare, and elsewhere,
// an external test package (core_test) included, one selected from the
// package under whatever name the file imports it as (core.OpenAOF). A method
// of the same name, used through a value (e.OpenAOF), is the engine's own and
// is not a use.
func TestDefaultEngineUsersAreCensused(t *testing.T) {
	t.Parallel()
	engineFunctions := sharedFunctions(t, ".", "defaultEngine")
	spaceFunctions := sharedFunctions(t, filepath.Join("..", "data_structure"), "DefaultSpace")
	found := map[string]string{}
	for _, pkg := range []struct{ dir, name string }{
		{".", "internal/core"},
		{filepath.Join("..", "server"), "internal/server"},
		{filepath.Join("..", "..", "cmd", "keel"), "cmd/keel"},
	} {
		files, err := filepath.Glob(filepath.Join(pkg.dir, "*.go"))
		require.NoError(t, err)
		require.NotEmpty(t, files, "no source in %s", pkg.dir)
		fset := token.NewFileSet()
		for _, name := range files {
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			// This package's own code names its functions bare; an external
			// test package (package core_test) selects them from an import.
			// Its non-test functions over the default engine are the default
			// engine; only its tests are users of it.
			own := pkg.dir == "." && f.Name.Name == "core"
			if own && !strings.HasSuffix(name, "_test.go") {
				continue
			}
			imports := importNames(f)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if via := defaultEngineUse(fn.Body, own, imports, engineFunctions, spaceFunctions); via != "" {
					found[pkg.name+": "+funcName(fn)] = via
				}
			}
		}
	}

	var unlisted, gone []string
	for user, via := range found {
		if _, ok := defaultEngineUsers[user]; !ok {
			unlisted = append(unlisted, user+" (through "+via+")")
		}
	}
	for user, part := range defaultEngineUsers {
		if _, ok := found[user]; !ok {
			gone = append(gone, user+" ("+part+")")
		}
	}
	sort.Strings(unlisted)
	sort.Strings(gone)
	for _, user := range unlisted {
		t.Errorf("%s uses the default engine, which plan step 2.7 removes; run it on an engine "+
			"of its own, or on the engine the server is handed, or list it with the part that moves it", user)
	}
	for _, user := range gone {
		t.Errorf("defaultEngineUsers lists %s, which no longer uses the default engine; remove the entry", user)
	}
}

// The import paths of the packages whose functions act on the default engine
// and on its space.
const (
	corePath          = "github.com/brandopakel/keel/internal/core"
	dataStructurePath = "github.com/brandopakel/keel/internal/data_structure"
)

// importNames maps each name a file refers to an imported package by to that
// package's path: its alias, or the last element of its path.
func importNames(f *ast.File) map[string]string {
	names := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = path
	}
	return names
}

// defaultEngineUse returns the first use of the default engine in body, or
// "". In this package's own code (own), that is defaultEngine by name or one
// of its package functions by bare name; elsewhere, one of those selected
// from core under the name the file imports it as (imports); and anywhere,
// DefaultSpace or one of data_structure's functions over it.
func defaultEngineUse(body ast.Node, own bool, imports map[string]string, engineFunctions, spaceFunctions map[string]bool) string {
	var found string
	// A selector's name is a field or a method, never the package function
	// it may share a name with: e.OpenAOF is not OpenAOF.
	selected := map[*ast.Ident]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			selected[x.Sel] = true
			pkg, ok := x.X.(*ast.Ident)
			if !ok {
				break
			}
			switch path := imports[pkg.Name]; {
			case path == dataStructurePath && (x.Sel.Name == "DefaultSpace" || spaceFunctions[x.Sel.Name]):
				found = pkg.Name + "." + x.Sel.Name
			case path == corePath && engineFunctions[x.Sel.Name]:
				found = pkg.Name + "." + x.Sel.Name
			}
		case *ast.CompositeLit:
			// A struct field's name in a composite literal is not a function.
			// A map's keys are expressions, which may name one, and a
			// literal's type is a map only when written as one here; a named
			// map type's identifier keys would be missed, which no test uses.
			if _, isMap := x.Type.(*ast.MapType); isMap {
				break
			}
			for _, elt := range x.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						selected[key] = true
					}
				}
			}
		case *ast.Ident:
			if own && !selected[x] && (x.Name == "defaultEngine" || engineFunctions[x.Name]) {
				found = x.Name
			}
		}
		return true
	})
	return found
}

// funcName is fn's name, with its receiver's type for a method:
// client.respond.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
