package takes

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// The Takes page and the pairing page speak Chinese or English, by the
// html's lang that lang.js sets. These tests hold every Chinese word of the
// pages to an English one: in the scripts every Chinese literal stands in
// the first argument of tr(zh, en); in the html every Chinese text or
// attribute carries data-en or data-en-<attr>, which the script puts in
// place, and the title is the script's tr.

// zh says whether r is written Chinese: a Han character, CJK punctuation
// (「」、。) or a fullwidth form (：，（）).
func zh(r rune) bool {
	return unicode.Is(unicode.Han, r) || (r >= 0x3000 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef)
}

func hasZH(s string) bool { return strings.ContainsFunc(s, zh) }

// span is a stretch of a script, [from, to) in bytes.
type span struct{ from, to int }

// zhOutsideTr scans a script and returns the Chinese it holds outside the
// first argument of a tr call, each as "line N: text": a string or regexp
// literal, or a bare character. Comments are skipped; a tr call without a
// second argument is an error too.
func zhOutsideTr(src string) (bad []string, calls int) {
	type frame struct {
		tr   bool
		from int // the first argument's start
		to   int // its end, the first top-level comma; -1 before it
	}
	var (
		stack   []frame
		trs     []span
		lits    []span
		prev    byte   // the last significant character, 0 at the start
		word    string // the identifier prev ends, if any
		pending bool   // a tr identifier stands before the next (
		lineOf  = func(i int) int { return strings.Count(src[:i], "\n") + 1 }
		isIdent = func(c byte) bool {
			return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		}
	)
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return append(bad, fmt.Sprintf("line %d: unclosed comment", lineOf(i))), calls
			}
			i += end + 4
			continue
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if c == '`' && strings.Contains(src[i:j], "${") {
				bad = append(bad, fmt.Sprintf("line %d: a template literal with ${}: the scan does not follow it", lineOf(i)))
			}
			lits = append(lits, span{i, j + 1})
			i, prev, word = j+1, c, ""
			continue
		case c == '/' && (prev == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", prev) >= 0 || slices.Contains([]string{"return", "typeof", "case", "in", "of", "new", "delete", "void", "throw"}, word)):
			// a regexp literal: to the / that is not escaped nor in a class
			j, class := i+1, false
			for j < len(src) && (class || src[j] != '/') {
				switch src[j] {
				case '\\':
					j++
				case '[':
					class = true
				case ']':
					class = false
				}
				j++
			}
			for j++; j < len(src) && isIdent(src[j]); j++ {
			}
			lits = append(lits, span{i, j})
			i, prev, word = j, '/', ""
			continue
		case isIdent(c):
			j := i
			for j < len(src) && isIdent(src[j]) {
				j++
			}
			w := src[i:j]
			if w == "tr" && prev != '.' {
				k := j
				for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n') {
					k++
				}
				pending = k < len(src) && src[k] == '('
			}
			i, prev, word = j, src[j-1], w
			continue
		case c == '(' || c == '[' || c == '{':
			stack = append(stack, frame{tr: pending && c == '(', from: i + 1, to: -1})
			pending = false
		case c == ',':
			if n := len(stack); n > 0 && stack[n-1].tr && stack[n-1].to < 0 {
				stack[n-1].to = i
			}
		case c == ')' || c == ']' || c == '}':
			n := len(stack)
			if n == 0 {
				return append(bad, fmt.Sprintf("line %d: unbalanced %c", lineOf(i), c)), calls
			}
			f := stack[n-1]
			stack = stack[:n-1]
			if f.tr {
				calls++
				if f.to < 0 {
					bad = append(bad, fmt.Sprintf("line %d: tr without an English argument", lineOf(i)))
				} else {
					trs = append(trs, span{f.from, f.to})
				}
			}
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(src[i:])
			if zh(r) {
				lits = append(lits, span{i, i + size})
			}
			i += size
			prev, word = 'x', ""
			continue
		}
		prev, word = c, ""
		i++
	}
	if len(stack) > 0 {
		bad = append(bad, fmt.Sprintf("line %d: unclosed bracket", lineOf(stack[0].from)))
	}
	for _, l := range lits {
		text := src[l.from:l.to]
		if !hasZH(text) || slices.ContainsFunc(trs, func(t span) bool { return t.from <= l.from && l.to <= t.to }) {
			continue
		}
		bad = append(bad, fmt.Sprintf("line %d: %s", lineOf(l.from), text))
	}
	return bad, calls
}

// TestPageScriptsSpeakBoth fails on a Chinese literal of app.js or
// pair.js that has no English beside it in a tr call.
func TestPageScriptsSpeakBoth(t *testing.T) {
	for _, name := range []string{"app.js", "pair.js"} {
		b, err := page.ReadFile("page/" + name)
		if err != nil {
			t.Fatal(err)
		}
		bad, calls := zhOutsideTr(string(b))
		for _, s := range bad {
			t.Errorf("%s %s", name, s)
		}
		if calls < 10 {
			t.Errorf("%s: %d tr calls; the scan lost its way", name, calls)
		}
	}
}

// TestZHOutsideTr holds the scan to what it must tell apart.
func TestZHOutsideTr(t *testing.T) {
	for src, want := range map[string]int{
		`x = tr("已复制 " + n + " 字", "Copied " + n);`:        0,
		`x = tr({ a: "送达" }, { a: "Sent" });`:              0,
		`x = "未送达"; // 未送达 in a comment is fine`:           1,
		`x = tr("Sent", "未送达");`:                           1,
		`x = tr("未送达");`:                                   2, // no English, and so outside a first argument
		`x = a / 2 + "条" + b / 3;`:                         1,
		`if (/[\u3400-\u9fff]/.test(m)) y = tr("是", "y");`: 0,
		`if (/[㐀-鿿]/.test(m)) y = 1;`:                      1,
		`x = /（/.test(s);`:                                 1,
		`/* 注释 */ x = foo.tr("未送达", "x");`:                 1, // a method, not the helper
	} {
		if bad, _ := zhOutsideTr(src); len(bad) != want {
			t.Errorf("%s: %d findings %q, want %d", src, len(bad), bad, want)
		}
	}
}

// TestPageHTMLSpeaksBoth fails on Chinese in index.html or pair.html
// without its English: data-en on an element of text alone, data-en-<attr>
// beside an attribute, and the title set by the page's script through tr.
// The language comes from lang.js, loaded before the page's script, over
// the html's own zh-CN.
func TestPageHTMLSpeaksBoth(t *testing.T) {
	for _, p := range []struct{ html, js, title string }{
		{"index.html", "app.js", "Recordings · megavoice"},
		{"pair.html", "pair.js", "iPhone Remote · megavoice"},
	} {
		b, err := page.ReadFile("page/" + p.html)
		if err != nil {
			t.Fatal(err)
		}
		js, err := page.ReadFile("page/" + p.js)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := html.Parse(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var scripts []string
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			switch n.Type {
			case html.ElementNode:
				at := map[string]string{}
				for _, a := range n.Attr {
					at[a.Key] = a.Val
				}
				if n.Data == "html" && at["lang"] != "zh-CN" {
					t.Errorf("%s: <html lang=%q>, want zh-CN", p.html, at["lang"])
				}
				if n.Data == "script" {
					scripts = append(scripts, at["src"])
				}
				for _, a := range n.Attr {
					if !strings.HasPrefix(a.Key, "data-en") && hasZH(a.Val) && at["data-en-"+a.Key] == "" {
						t.Errorf("%s: <%s %s=%q> has no data-en-%s", p.html, n.Data, a.Key, a.Val, a.Key)
					}
				}
				if _, ok := at["data-en"]; ok {
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type != html.TextNode {
							t.Errorf("%s: <%s data-en> holds an element, which its English would replace", p.html, n.Data)
						}
					}
				}
			case html.TextNode:
				if !hasZH(n.Data) {
					break
				}
				text := strings.TrimSpace(n.Data)
				if n.Parent.Data == "title" {
					if call := `document.title = tr("` + text + `", "` + p.title + `");`; !strings.Contains(string(js), call) {
						t.Errorf("%s: %s does not set the title with %s", p.html, p.js, call)
					}
				} else if !slices.ContainsFunc(n.Parent.Attr, func(a html.Attribute) bool { return a.Key == "data-en" && a.Val != "" }) {
					t.Errorf("%s: %q in <%s> has no data-en", p.html, text, n.Parent.Data)
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
		if !slices.Equal(scripts, []string{"lang.js", p.js}) {
			t.Errorf("%s: scripts %q, want lang.js before %s", p.html, scripts, p.js)
		}
	}
}
