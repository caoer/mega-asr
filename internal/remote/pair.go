package remote

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/l10n"
	"rsc.io/qr"
)

// Pairing is a live pairing: the URL its QR code carries and when it ends.
type Pairing struct {
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
	QR      QR        `json:"qr"`
}

// QR is a QR code's modules, row by row, "1" dark: what the pairing page
// draws.
type QR struct {
	Size int    `json:"size"`
	Bits string `json:"bits"`
}

// PairURL is the URL a pairing QR code carries (doc.go).
func PairURL(name string, port int, fp, secret string, ips []string) string {
	q := url.Values{}
	q.Set("v", "1")
	q.Set("name", name)
	q.Set("port", strconv.Itoa(port))
	q.Set("fp", fp)
	q.Set("s", secret)
	if len(ips) > 0 {
		q.Set("ip", strings.Join(ips, ","))
	}
	return "megavoice://pair?" + q.Encode()
}

// NewPairing opens a pairing window with a fresh secret, replacing any live
// one: for PairLife, one phone that sends the secret is paired.
func (s *Server) NewPairing() (Pairing, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Pairing{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	addr, name := s.addr, s.macName()
	s.mu.Unlock()
	if addr == "" {
		return Pairing{}, errors.New("the phones' listener is not up")
	}
	_, ps, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(ps)
	p := Pairing{URL: PairURL(name, port, s.fp, secret, lanIPs()), Expires: s.now().Add(PairLife)}
	code, err := qr.Encode(p.URL, qr.M)
	if err != nil {
		return Pairing{}, err
	}
	p.QR = qrOf(code)
	s.mu.Lock()
	s.pairing = &pairWindow{secret: secret, pairing: p}
	s.mu.Unlock()
	s.logf("remote: pairing open until %s", p.Expires.Format(time.TimeOnly))
	return p, nil
}

func qrOf(c *qr.Code) QR {
	var b strings.Builder
	b.Grow(c.Size * c.Size)
	for y := range c.Size {
		for x := range c.Size {
			if c.Black(x, y) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
	}
	return QR{Size: c.Size, Bits: b.String()}
}

// lanIPs are this Mac's IPv4 addresses a phone may reach: up, non-loopback,
// not link-local; Wi-Fi and Ethernet (en*) first, at most four.
func lanIPs() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	type cand struct {
		ip string
		en bool
	}
	var cs []cand
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLoopback() {
				continue
			}
			cs = append(cs, cand{ip.String(), strings.HasPrefix(i.Name, "en")})
		}
	}
	slices.SortStableFunc(cs, func(a, b cand) int {
		switch {
		case a.en == b.en:
			return 0
		case a.en:
			return -1
		}
		return 1
	})
	var out []string
	for _, c := range cs {
		if len(out) < 4 {
			out = append(out, c.ip)
		}
	}
	return out
}

// view is the pairing page's state.
type view struct {
	Enabled   bool       `json:"enabled"`
	Why       string     `json:"why,omitempty"` // why phones cannot reach this Mac
	Name      string     `json:"name,omitempty"`
	Addr      string     `json:"addr,omitempty"`
	FP        string     `json:"fp,omitempty"`
	Advertise string     `json:"advertise,omitempty"` // why the service is not advertised
	Pairing   *Pairing   `json:"pairing"`
	Phones    []Phone    `json:"phones"`
	Relay     *RelayView `json:"relay"`
}

// PageHandler answers the pairing page's API under /takes/api/remote on the
// loopback listener, whose guard has checked the key: GET the state, POST
// pair for a new QR code, POST revoke {"name"}. s nil: phones are off, and
// why says so.
func PageHandler(s *Server, why string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /takes/api/remote", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.view(why))
	})
	mux.HandleFunc("POST /takes/api/remote/pair", func(w http.ResponseWriter, r *http.Request) {
		if s == nil {
			httpErr(w, http.StatusConflict, why)
			return
		}
		if _, err := s.NewPairing(); err != nil {
			httpErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, s.view(why))
	})
	mux.HandleFunc("POST /takes/api/remote/revoke", func(w http.ResponseWriter, r *http.Request) {
		var a struct {
			Name string `json:"name"`
		}
		if s == nil {
			httpErr(w, http.StatusConflict, why)
			return
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&a); err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Revoke(a.Name); err != nil {
			httpErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, s.view(why))
	})
	return mux
}

func (s *Server) view(why string) view {
	if s == nil {
		return view{Why: why, Phones: []Phone{}, Relay: &RelayView{}}
	}
	v := view{Enabled: true, FP: s.fp, Phones: s.Phones(), Relay: s.relayView()}
	s.mu.Lock()
	v.Name, v.Addr = s.macName(), s.addr
	if s.serveErr != nil {
		v.Why = l10n.T("收不到 iPhone 的连接：", "Can't take iPhone connections: ") + s.serveErr.Error()
	}
	if s.advertErr != nil {
		v.Advertise = s.advertErr.Error()
	}
	if w := s.pairWindowLocked(); w != nil {
		p := w.pairing
		v.Pairing = &p
	}
	s.mu.Unlock()
	return v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
