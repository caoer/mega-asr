package l10n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The packages whose strings a person reads in the app: menus, alerts,
// prompts, the overlay, the update and download lines, the Takes and
// pairing pages' server side, and what a paired phone shows.
var userFacing = []string{
	"../../cmd/megavoice",
	"../session",
	"../remote",
	"../relay/client",
	"../mac",
	"../takes",
	"../deliver",
	"../app",
	"../update",
}

// notShown are Chinese strings in those packages that are not UI text of
// their own, by file and string, with why.
var notShown = map[[2]string]string{
	{"../update/update.go", "更新不可用，请从页面重新安装"}: "mega-update's UserMessage, carried in-tree unchanged; the app shows cmd/megavoice userMessage",
	{"../update/update.go", "更新校验失败，下次再试"}:    "the same",
	{"../update/update.go", "请从页面重新安装"}:       "the same",
	{"../takes/takes.go", "录音历史 · megavoice"}: "PageTitle: the window title the resend picker matches; page/app.js shows it or PageTitleEN",
}

// TestCallsSpeakBoth: every Chinese string literal in the user-facing
// packages is half of an l10n pair, and every pair T/F/Err(orig, other)
// carries exactly one Chinese and one other text, neither empty, with the
// same format verbs. Together with the menu tests' English and Chinese
// renderings, this is what keeps every string in both languages.
func TestCallsSpeakBoth(t *testing.T) {
	fset := token.NewFileSet()
	n := 0
	for _, dir := range userFacing {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: no Go files (%v)", dir, err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			covered := map[*ast.BasicLit]bool{}
			ast.Inspect(f, func(node ast.Node) bool {
				var call *ast.CallExpr
				switch x := node.(type) {
				case *ast.CallExpr:
					if !isPair(x) {
						return true
					}
					call = x
				case *ast.CompositeLit: // l10n.Pair{orig, other}, as a T call's arguments
					if !isL10n(x.Type, "Pair") {
						return true
					}
					call = &ast.CallExpr{Fun: &ast.SelectorExpr{X: ast.NewIdent("l10n"), Sel: ast.NewIdent("Pair")}, Lparen: x.Lbrace, Args: x.Elts}
				default:
					return true
				}
				n++
				where := fset.Position(call.Pos()).String()
				if len(call.Args) < 2 {
					t.Errorf("%s: an l10n pair with %d arguments", where, len(call.Args))
					return true
				}
				orig, other := lits(call.Args[0]), lits(call.Args[1])
				for _, l := range append(slices.Clone(orig), other...) {
					covered[l] = true
				}
				origZH, otherZH := chinese(orig), chinese(other)
				switch {
				case len(orig) == 0 || len(other) == 0:
					t.Errorf("%s: a side of the pair has no text of its own", where)
				case origZH == otherZH:
					t.Errorf("%s: %q / %q: want one Chinese and one English text", where, text(orig), text(other))
				case text(orig) == "" || text(other) == "":
					t.Errorf("%s: an empty text in the pair", where)
				}
				if sel := call.Fun.(*ast.SelectorExpr).Sel.Name; sel == "F" {
					if a, b := verbs(text(orig)), verbs(text(other)); !slices.Equal(a, b) {
						t.Errorf("%s: format verbs %v and %v differ", where, a, b)
					}
				}
				return true
			})
			ast.Inspect(f, func(node ast.Node) bool {
				l, ok := node.(*ast.BasicLit)
				if !ok || l.Kind != token.STRING || covered[l] {
					return true
				}
				s, err := strconv.Unquote(l.Value)
				if err != nil || !Chinese(s) {
					return true
				}
				if _, ok := notShown[[2]string{path, s}]; ok {
					return true
				}
				t.Errorf("%s: %q is Chinese only: make it an l10n pair", fset.Position(l.Pos()), s)
				return true
			})
		}
	}
	if n < 150 {
		t.Errorf("found %d l10n pairs; the scan is not reading the packages it should", n)
	}
}

// isPair is a call l10n.T, l10n.F or l10n.Err.
func isPair(c *ast.CallExpr) bool { return isL10n(c.Fun, "T", "F", "Err") }

// isL10n is e an l10n.<name> for one of names.
func isL10n(e ast.Expr, names ...string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "l10n" && slices.Contains(names, sel.Sel.Name)
}

// lits are the string literals of an argument, in order.
func lits(e ast.Expr) []*ast.BasicLit {
	var out []*ast.BasicLit
	ast.Inspect(e, func(n ast.Node) bool {
		if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
			out = append(out, l)
		}
		return true
	})
	return out
}

func text(ls []*ast.BasicLit) string {
	var b strings.Builder
	for _, l := range ls {
		s, _ := strconv.Unquote(l.Value)
		b.WriteString(s)
	}
	return b.String()
}

func chinese(ls []*ast.BasicLit) bool { return Chinese(text(ls)) }

var verb = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z%]`)

// verbs are a format's verbs, sorted, each once.
func verbs(s string) []string {
	v := verb.FindAllString(s, -1)
	slices.Sort(v)
	return slices.Compact(v)
}
