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

// processWide are the calls that change, or measure, what every test in the
// process shares: the log's output, the heap and the collector, every
// goroutine's stack, the environment, the working directory, resource limits
// and signals. A test
// that makes one stays serial: Go runs the serial tests of a package before it
// starts the parallel ones, and never beside them.
var processWide = map[string]map[string]bool{
	"log":     {"SetOutput": true, "SetFlags": true, "SetPrefix": true},
	"runtime": {"ReadMemStats": true, "GC": true, "GOMAXPROCS": true, "Stack": true, "NumGoroutine": true},
	"testing": {"AllocsPerRun": true, "Benchmark": true},
	"debug":   {"SetGCPercent": true, "SetMemoryLimit": true, "FreeOSMemory": true, "SetMaxThreads": true},
	"os":      {"Setenv": true, "Unsetenv": true, "Chdir": true},
	"syscall": {"Setrlimit": true},
	"signal":  {"Notify": true, "Ignore": true, "Reset": true},
}

// TestParallelTestsShareNoPackageState: a test that runs in parallel runs on
// an engine of its own (plan step 2.6), and there is no other since step 2.7.
// It must not write to the package's variables, whether by name or through an
// index, a field or a pointer (commandTable["X"] = ...), or delete from or
// clear one, nor make a process-wide call (processWide), itself or through a
// test helper that does. Go would run such a test beside others that do the
// same, so that they shared one table, log or heap reading, and the race
// detector finds only some of that, when the timing shows it.
//
// The check reads source, not types. It follows package-level functions and
// variables by name, and the methods of the test files' own types by method
// name. The package's own methods it does not follow: a method acts on the
// engine it is called on, and the engine census keeps the package's
// variables to tables and sentinels nothing writes.
func TestParallelTestsShareNoPackageState(t *testing.T) {
	t.Parallel()
	census := newParallelCensus(t)
	var parallel int
	for _, test := range census.src.tests {
		if !callsParallel(test) {
			continue
		}
		parallel++
		if via := census.reaches(test.Body, true); via != "" {
			t.Errorf("%s runs in parallel and reaches what the process shares: %s; "+
				"run it on newTestEngine with the helpers that take an engine, or keep it serial and say why",
				test.Name.Name, census.chain(via))
		}
	}
	require.NotZero(t, parallel, "the census found no parallel test to check")
}

// parallelCensus knows, for each name in the package, whether it reaches what
// the process shares, and through what.
type parallelCensus struct {
	src packageSource
	// why holds each package-level name that reaches what the process shares,
	// and each test method's name prefixed with ".", with what it reaches it
	// through.
	why map[string]string
}

func newParallelCensus(t *testing.T) *parallelCensus {
	t.Helper()
	c := &parallelCensus{src: readPackage(t, "."), why: map[string]string{}}
	for changed := true; changed; {
		changed = false
		for name, bodies := range c.src.named {
			if _, done := c.why[name]; done {
				continue
			}
			for _, b := range bodies {
				if via := c.reaches(b.node, b.test); via != "" {
					c.why[name], changed = via, true
					break
				}
			}
		}
	}
	return c
}

// reaches returns the first name in n that reaches what the process shares,
// or "". Only test code calls the test files' methods, so only there does a
// selector's name stand for one (methods).
func (c *parallelCensus) reaches(n ast.Node, methods bool) string {
	var found string
	// A selector's name is a field or a method, never the package function it
	// may share a name with: e.OpenAOF is not OpenAOF.
	selected := map[*ast.Ident]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			// A test file's method, called: by name alone, since the
			// census has no types, but only where it is called, so that a
			// field of the same name is not taken for it.
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && methods {
				if _, ok := c.why["."+sel.Sel.Name]; ok {
					found = "." + sel.Sel.Name
					return false
				}
			}
		case *ast.KeyValueExpr:
			// A field's name in a composite literal, such as run: in
			// goldenScenario{run: goldenSession}, is not the function run.
			if key, ok := x.Key.(*ast.Ident); ok {
				selected[key] = true
			}
		case *ast.SelectorExpr:
			selected[x.Sel] = true
			if pkg, ok := x.X.(*ast.Ident); ok && c.src.imports[pkg.Name] {
				if processWide[pkg.Name][x.Sel.Name] {
					found = pkg.Name + "." + x.Sel.Name
				}
				return false
			}
		case *ast.Ident:
			if selected[x] || c.src.local(x) {
				break
			}
			if _, ok := c.why[x.Name]; ok && c.src.declared[x.Name] {
				found = x.Name
			}
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				break
			}
			for _, lhs := range x.Lhs {
				if name := c.packageVarIn(lhs); name != "" {
					found = name + " ="
				}
			}
		case *ast.IncDecStmt:
			if name := c.packageVarIn(x.X); name != "" {
				found = name + x.Tok.String()
			}
		}
		// delete and clear change the map or slice they are handed.
		if call, ok := n.(*ast.CallExpr); ok && found == "" && len(call.Args) > 0 {
			if fn, ok := call.Fun.(*ast.Ident); ok && (fn.Name == "delete" || fn.Name == "clear") {
				if name := c.packageVarIn(call.Args[0]); name != "" {
					found = fn.Name + "(" + name + ")"
				}
			}
		}
		return true
	})
	return found
}

// packageVarIn names the package variable that x writes into, through any
// index, field or pointer - commandTable["X"], a variable's field or
// *p for a package-level p - or returns "" when x is rooted elsewhere.
func (c *parallelCensus) packageVarIn(x ast.Expr) string {
	for {
		switch y := x.(type) {
		case *ast.IndexExpr:
			x = y.X
		case *ast.IndexListExpr:
			x = y.X
		case *ast.SelectorExpr:
			if pkg, ok := y.X.(*ast.Ident); ok && c.src.imports[pkg.Name] {
				return "" // another package's variable; processWide covers the ones that matter
			}
			x = y.X
		case *ast.StarExpr:
			x = y.X
		case *ast.ParenExpr:
			x = y.X
		case *ast.Ident:
			if c.src.packageVars[y.Name] && !c.src.local(y) {
				return y.Name
			}
			return ""
		default:
			return ""
		}
	}
}

// chain spells out how via reaches what the process shares.
func (c *parallelCensus) chain(via string) string {
	path := []string{strings.TrimPrefix(via, ".")}
	for seen := map[string]bool{}; c.why[via] != "" && !seen[via]; via = c.why[via] {
		seen[via] = true
		path = append(path, strings.TrimPrefix(c.why[via], "."))
	}
	return strings.Join(path, " -> ")
}

// callsParallel says whether a test calls t.Parallel, for itself or in a
// subtest.
func callsParallel(fn *ast.FuncDecl) bool {
	var found bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 0 {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" {
				found = true
			}
		}
		return !found
	})
	return found
}

// packageSource is what the census reads of one package's files, tests
// included.
type packageSource struct {
	// named holds the bodies of package-level functions and variables by
	// name, and of the test files' methods by "." + name.
	named map[string][]sourceBody
	// declared are the package-level names; packageVars the variables the
	// package's own code, not its tests, declares; imports the names the
	// files import packages under.
	declared, packageVars, imports map[string]bool
	tests                          []*ast.FuncDecl
	// topLevel are the objects the parser resolved package-level
	// declarations to, file by file.
	topLevel map[*ast.Object]bool
}

// local says whether id names something declared inside a function, such as
// a variable called run or commands, rather than the package-level name it
// shadows. The parser resolves names within a file: one declared in another
// file of the package is left unresolved, and so is not local either.
func (src packageSource) local(id *ast.Ident) bool {
	// Ident.Obj is deprecated because syntax alone cannot resolve every name;
	// a census that reads syntax has nothing better, and a name it cannot
	// resolve it treats as the package's, which errs towards reporting.
	return id.Obj != nil && !src.topLevel[id.Obj]
}

// sourceBody is a function's body or a variable's value, and whether a test
// file holds it.
type sourceBody struct {
	node ast.Node
	test bool
}

func readPackage(t *testing.T, dir string) packageSource {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	src := packageSource{named: map[string][]sourceBody{}, declared: map[string]bool{},
		packageVars: map[string]bool{}, imports: map[string]bool{}, topLevel: map[*ast.Object]bool{}}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		isTest := strings.HasSuffix(name, "_test.go")
		for _, obj := range f.Scope.Objects {
			src.topLevel[obj] = true
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			local := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			src.imports[local] = true
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				key := d.Name.Name
				switch {
				case d.Recv != nil && !isTest:
					// The package's own methods act on the engine they
					// are called on; the census does not follow them.
					continue
				case d.Recv != nil:
					key = "." + key
				case isTest && strings.HasPrefix(key, "Test"):
					src.declared[key] = true
					src.tests = append(src.tests, d)
					continue
				default:
					src.declared[key] = true
				}
				src.named[key] = append(src.named[key], sourceBody{d.Body, isTest})
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, id := range vs.Names {
						if id.Name == "_" {
							continue
						}
						src.declared[id.Name] = true
						if !isTest {
							src.packageVars[id.Name] = true
						}
						if i < len(vs.Values) {
							src.named[id.Name] = append(src.named[id.Name], sourceBody{vs.Values[i], isTest})
						}
					}
				}
			}
		}
	}
	sort.Slice(src.tests, func(i, j int) bool { return src.tests[i].Name.Name < src.tests[j].Name.Name })
	return src
}
