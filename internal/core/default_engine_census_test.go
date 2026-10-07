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
// tests a package function is one of its names used bare, and elsewhere one
// selected from the package (core.OpenAOF). A method of the same name, used
// through a value (e.OpenAOF), is the engine's own and is not a use.
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
			// This package's own functions over the default engine are the
			// default engine; only its tests are users of it.
			own := pkg.dir == "."
			if own && !strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if via := defaultEngineUse(fn.Body, own, engineFunctions, spaceFunctions); via != "" {
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

// defaultEngineUse returns the first use of the default engine in body, or
// "". In this package (own), that is defaultEngine by name or one of its
// package functions by bare name; elsewhere, one of those selected from core;
// and anywhere, DefaultSpace or one of data_structure's functions over it.
func defaultEngineUse(body ast.Node, own bool, engineFunctions, spaceFunctions map[string]bool) string {
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
			switch {
			case pkg.Name == "data_structure" && (x.Sel.Name == "DefaultSpace" || spaceFunctions[x.Sel.Name]):
				found = "data_structure." + x.Sel.Name
			case !own && pkg.Name == "core" && engineFunctions[x.Sel.Name]:
				found = "core." + x.Sel.Name
			}
		case *ast.KeyValueExpr:
			// A field's name in a composite literal is not a function.
			if key, ok := x.Key.(*ast.Ident); ok {
				selected[key] = true
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
