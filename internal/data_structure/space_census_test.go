package data_structure

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// packageVars is every package-level variable this package may declare, with
// the reason each one is allowed.
//
// The embedding plan (docs/embedding-plan.md) moves mutable package state into
// a Space, and later an engine, so that two instances in one process share
// nothing. A new package variable would quietly undo that, and nothing would
// notice until two instances disagreed. So the census below parses this
// package's source and fails on any top-level variable missing from this list.
//
// The list holds DefaultSpace and values that are computed once and never
// written again. It is meant to shrink as the plan proceeds - DefaultSpace goes
// when every engine owns its own space - and an entry whose variable is gone
// fails the census too, so the list cannot outlive what it excuses.
var packageVars = map[string]string{
	"layoutOnly":          "a layout control build only; never merged",
	"DefaultSpace":        "the server's space until engines own theirs (plan step 2.7)",
	"keyLookupSeed":       "random per process and never written again, so every space can hash with it",
	"morrisValue":         "a table computed from morrisA in init and only read after",
	"morrisProb":          "a table computed from morrisA in init and only read after",
	"MorrisMaxCount":      "computed from morrisValue in init and only read after",
	"MorrisRelativeError": "a constant that Go cannot declare as one, because it calls math.Sqrt",
	"alphaInf":            "a constant that Go cannot declare as one, because it calls math.Log",
	"ErrFilterTooLarge":   "an error sentinel, matched by identity and never reassigned",
	"ErrNonScalingFull":   "an error sentinel, matched by identity and never reassigned",
	"errShortPayload":     "an error sentinel, matched by identity and never reassigned",
}

// TestPackageStateIsCensused reads the package's own declarations rather than
// trusting review to spot a new global. Test files are left out: what they
// declare is not in the package a caller links.
func TestPackageStateIsCensused(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					if id.Name == "_" {
						continue // an interface assertion holds nothing
					}
					declared[id.Name] = true
					if _, ok := packageVars[id.Name]; !ok {
						t.Errorf("%s: package variable %s would be shared by every Space in the process; "+
							"make it a Space field, or list it in packageVars with the reason it never changes",
							fset.Position(id.Pos()), id.Name)
					}
				}
			}
		}
	}
	require.NotEmpty(t, declared, "the census found no source to read")
	for name := range packageVars {
		if !declared[name] {
			t.Errorf("packageVars lists %s, which is no longer declared; remove the entry", name)
		}
	}
}
