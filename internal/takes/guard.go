package takes

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Denied is the body of every refusal: where the page is opened from.
const Denied = "从菜单栏重新打开录音历史\n"

// KeyFile is the listener's key: 128 random bits, hex, in a 0600 file of a
// 0700 state directory. It changes on Rotate: megavoice serve rotates it at
// every start and whenever its listener stops, and `megavoice takes url
// --rotate` does; an open tab is refused after a restart of megavoice and is
// reopened from the menu.
type KeyFile string

// Load returns the key, creating the file on first use. It makes the state
// directory 0700 whatever it was, and replaces a key file others could read
// with a new key.
func (k KeyFile) Load() (string, error) {
	if err := private(filepath.Dir(string(k))); err != nil {
		return "", err
	}
	st, err := os.Stat(string(k))
	if err == nil && st.Mode().Perm()&0o077 != 0 {
		return k.Rotate()
	}
	b, err := os.ReadFile(string(k))
	if errors.Is(err, fs.ErrNotExist) {
		key := newKey()
		f, err := os.OpenFile(string(k), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) { // another process created it first
			return k.Load()
		}
		if err != nil {
			return "", err
		}
		_, err = f.WriteString(key + "\n")
		return key, errors.Join(err, f.Close())
	}
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if len(key) != 32 {
		return "", fmt.Errorf("%s: not a key (megavoice takes url --rotate writes a new one)", k)
	}
	return key, nil
}

// Rotate writes a new key in place of the old one and returns it: every
// open page and every address handed out before is refused from then on.
// The new file is 0600 whatever the old one was.
func (k KeyFile) Rotate() (string, error) {
	dir := filepath.Dir(string(k))
	if err := private(dir); err != nil {
		return "", err
	}
	key := newKey()
	f, err := os.CreateTemp(dir, filepath.Base(string(k))+".*.tmp") // 0600
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(key + "\n")
	if err = errors.Join(err, f.Close()); err == nil {
		err = os.Rename(f.Name(), string(k))
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return key, nil
}

// private makes dir, 0700, or makes an existing dir 0700.
func private(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

func newKey() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// URL is the address that opens path on the listener at addr with the key
// in its fragment, which a browser never sends to a server. The page's
// script keeps the key in the tab's sessionStorage and sends it as Header.
func URL(addr, path, key string) string {
	return "http://" + addr + path + "#k=" + key
}

// Header is the request header that carries the listener's key.
const Header = "X-Megavoice-Key"

// Guard answers for everything the loopback listener serves. A request
// passes when its Host is a loopback address with the listener's port, its
// Sec-Fetch-Site is same-origin or none, and it carries the key as the one
// value of Header.
// The pages themselves (the files of the Takes page, and / which sends a
// browser on to it) hold no take and pass without the key: a browser sends
// no header of the page's choosing when it opens an address. No cookie is set or
// read: a browser sends a cookie of 127.0.0.1 to every port of it, while
// it keeps sessionStorage per origin, port included. A POST must be JSON.
// Every response, a refusal included, carries CSP, Referrer-Policy:
// no-referrer and X-Content-Type-Options: nosniff.
type Guard struct {
	Next   http.Handler
	Key    func() (string, error)
	Port   string // the listener's port: a Host must name it
	Policy string // Content-Security-Policy; Policy() builds it
}

func (g *Guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", g.Policy)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	if site := r.Header.Get("Sec-Fetch-Site"); !g.host(r.Host) || (site != "same-origin" && site != "none") {
		deny(w)
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && isPage(r.URL.Path) {
		g.Next.ServeHTTP(w, r)
		return
	}
	key, err := g.Key()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if vs := r.Header.Values(Header); len(vs) != 1 || !same(vs[0], key) {
		deny(w)
		return
	}
	if r.Method == http.MethodPost {
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			http.Error(w, "want application/json", http.StatusUnsupportedMediaType)
			return
		}
	}
	g.Next.ServeHTTP(w, r)
}

// isPage reports whether path is a page the listener serves without the
// key: /, the redirect to the Takes page, or a file of the page under
// /takes/.
func isPage(path string) bool {
	if path == "/" {
		return true
	}
	name, ok := strings.CutPrefix(path, "/takes/")
	if !ok {
		return false
	}
	if name == "" {
		name = "index.html"
	}
	st, err := fs.Stat(page, "page/"+name)
	return err == nil && !st.IsDir()
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func deny(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(Denied))
}

// host reports whether a request's Host is a loopback address with the
// listener's port.
func (g *Guard) host(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port != g.Port {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var inline = regexp.MustCompile(`(?s)<(script|style)>(.*?)</(?:script|style)>`)

// Policy is the Content-Security-Policy of the listener: the page's own
// origin and nothing else, plus the hash of each inline <script> and
// <style> block of pages (the Takes page has none). Audio plays from a
// blob: URL, since the page's script fetches it with the key.
func Policy(pages ...[]byte) string {
	var scripts, styles string
	for _, p := range pages {
		for _, m := range inline.FindAllSubmatch(p, -1) {
			sum := sha256.Sum256(m[2])
			h := " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if string(m[1]) == "script" {
				scripts += h
			} else {
				styles += h
			}
		}
	}
	return "default-src 'self'; script-src 'self'" + scripts + "; style-src 'self'" + styles +
		"; media-src 'self' blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}
