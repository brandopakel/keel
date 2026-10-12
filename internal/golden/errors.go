package golden

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrorClasses are the prefixes Redis's error replies begin with, and the
// prefixes of Keel's own families' errors.
var ErrorClasses = regexp.MustCompile(`^(ERR|WRONGTYPE|OOM|MISCONF|READONLY|MASTERDOWN|FENCED|EXECABORT|NOAUTH|NOPROTO|WRONGPASS|REPLTERM)\b|^(CMS|MORRIS|MEMKV): `)

// ErrorTexts reads the error texts a package's commands can answer with, from
// the non-test source files in dir: every string literal that begins with an
// error class; every literal an error is built from (errors.New, fmt.Errorf,
// errors.New(fmt.Sprintf(...))) as the argument of a call that encodes a
// reply (encode, Encode, EncodeAs, refuse, responseError); and, in a file
// that encodes replies, every literal an error is built from that begins
// with a capital letter, as RedisBloom's errors do and Go's own errors do
// not. Each text is returned cut at its first format verb, mapped to the
// literal it came from, so that a reply can be looked for by its fixed
// prefix and the literal named in a message.
func ErrorTexts(dir string) (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	texts := map[string]string{}
	add := func(lit *ast.BasicLit) {
		if lit == nil || lit.Kind != token.STRING {
			return
		}
		text, err := strconv.Unquote(lit.Value)
		if err != nil || text == "" {
			return
		}
		prefix, _, _ := strings.Cut(text, "%")
		texts[prefix] = text
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		encodes := strings.Contains(string(src), "encode(") || strings.Contains(string(src), "Encode(")
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					if text, err := strconv.Unquote(n.Value); err == nil && ErrorClasses.MatchString(text) {
						add(n)
					}
				}
			case *ast.CallExpr:
				if lit := errorLiteral(n); lit != nil && encodes {
					if text, err := strconv.Unquote(lit.Value); err == nil && text != "" && text[0] >= 'A' && text[0] <= 'Z' {
						add(lit)
					}
				}
				if encodesReply(n) {
					for _, arg := range n.Args {
						if call, ok := arg.(*ast.CallExpr); ok {
							add(errorLiteral(call))
						}
					}
				}
			}
			return true
		})
	}
	return texts, nil
}

// errorLiteral is the literal call builds an error from, for errors.New,
// fmt.Errorf and errors.New(fmt.Sprintf(...)); nil for any other call.
func errorLiteral(call *ast.CallExpr) *ast.BasicLit {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return nil
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil
	}
	switch {
	case pkg.Name == "errors" && sel.Sel.Name == "New", pkg.Name == "fmt" && sel.Sel.Name == "Errorf":
	default:
		return nil
	}
	switch arg := call.Args[0].(type) {
	case *ast.BasicLit:
		return arg
	case *ast.CallExpr:
		if inner, ok := arg.Fun.(*ast.SelectorExpr); ok && inner.Sel.Name == "Sprintf" && len(arg.Args) > 0 {
			if lit, ok := arg.Args[0].(*ast.BasicLit); ok {
				return lit
			}
		}
	}
	return nil
}

// encodesReply reports whether call writes its argument as a reply.
func encodesReply(call *ast.CallExpr) bool {
	var name string
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		name = fun.Sel.Name
	case *ast.Ident:
		name = fun.Name
	}
	switch name {
	case "encode", "Encode", "EncodeAs", "refuse", "responseError", "responseErrorRw":
		return true
	}
	return false
}

// ReplyTexts is every reply the cases hold, in both protocols, joined, so
// that an error text can be looked for among them: at the top of a reply or
// inside an array, EXEC's or BF.MADD's. An error reply inside the text is
// its class and words after a '-'.
func ReplyTexts(cases map[string]Case) string {
	var b strings.Builder
	for _, c := range cases {
		for _, s := range c.Steps {
			b.WriteString(s.Replies[0])
			b.WriteString(s.Replies[1])
		}
	}
	return b.String()
}

// Uncovered compares the texts a package can answer with against the replies
// a fixture holds, given the texts excused with a reason, and returns what
// is wrong: a text that is answered and excused, a text that is neither
// answered nor excused, and an excused text the source no longer holds.
func Uncovered(texts map[string]string, replies string, excused map[string]string) []string {
	var problems []string
	listed := map[string]bool{}
	for prefix, literal := range texts {
		covered := prefix != "" && strings.Contains(replies, "-"+prefix)
		reason, ok := excused[literal]
		listed[literal] = true
		switch {
		case covered && ok:
			problems = append(problems, strconv.Quote(literal)+" is answered in the fixture ("+reason+"); remove it from the excused texts")
		case !covered && !ok:
			problems = append(problems, strconv.Quote(literal)+" is never answered in the fixture; send a command that answers it, or excuse it with the reason")
		}
	}
	for literal := range excused {
		if !listed[literal] {
			problems = append(problems, strconv.Quote(literal)+" is excused, and the source no longer holds it; remove it")
		}
	}
	return problems
}
