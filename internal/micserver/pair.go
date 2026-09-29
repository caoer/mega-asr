package micserver

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
)

// Paired is what a Mac keeps of a pairing: the box's name, the certificate
// fingerprint the PIN confirmed, and the bearer token for its streams.
type Paired struct {
	Box         string
	Fingerprint string
	Token       string
}

// ErrRefused is Pair's error when the box refuses the PIN: wrong, expired,
// used, or out of tries — or when the answer is not the box's.
var ErrRefused = errors.New("pairing refused")

type startReq struct {
	Name string `json:"name"`
	A    []byte `json:"a"`
}

type startResp struct {
	ID string `json:"id"`
	B  []byte `json:"b"`
}

type confirmReq struct {
	ID      string `json:"id"`
	Confirm []byte `json:"confirm"`
}

type confirmResp struct {
	Box     string `json:"box"`
	Token   []byte `json:"token"` // sealed under a key of the PAKE session
	Confirm []byte `json:"confirm"`
}

// adFor is the PAKE's associated data: the certificate a side sees on the
// connection, which the box reads from its own key and the Mac from the
// TLS handshake. See the package doc.
func adFor(fp string) []byte { return []byte("megavoice-pair-v1 cert " + fp) }

// Pair pairs with the mic server at addr (host:port) as name, authenticated
// by pin alone: it confirms, by the PIN, the certificate it then pins.
func Pair(ctx context.Context, addr, pin, name string) (Paired, error) {
	if !nameRE.MatchString(name) {
		return Paired{}, fmt.Errorf("client name %q: letters, digits, '.', '_' and '-', up to 64", name)
	}
	fp, err := peerFingerprint(ctx, addr)
	if err != nil {
		return Paired{}, err
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: audio.PinnedTLS(fp)}}
	defer hc.CloseIdleConnections()
	msgA, ini, err := startPAKE(pin, name, adFor(fp))
	if err != nil {
		return Paired{}, err
	}
	var sr startResp
	if err := post(ctx, hc, addr, "/pair", startReq{Name: name, A: msgA}, &sr); err != nil {
		return Paired{}, err
	}
	confirm, key, err := ini.finish(sr.B)
	if err != nil {
		return Paired{}, fmt.Errorf("pairing: %w", err)
	}
	var cr confirmResp
	if err := post(ctx, hc, addr, "/pair/confirm", confirmReq{ID: sr.ID, Confirm: confirm}, &cr); err != nil {
		return Paired{}, err
	}
	token, err := macOpen(key, msgA, sr.B, cr.Box, name, cr.Token, cr.Confirm)
	if err != nil {
		return Paired{}, fmt.Errorf("%w: the answer is not from the box that showed the PIN", ErrRefused)
	}
	return Paired{Box: cr.Box, Fingerprint: fp, Token: token}, nil
}

// Probe checks that a mic server answers TLS at addr before the user is
// asked for a PIN; the error says why not (DialError).
func Probe(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := peerFingerprint(ctx, addr)
	return err
}

// peerFingerprint is the certificate addr presents; the PAKE then decides
// whether it is the box's.
func peerFingerprint(ctx context.Context, addr string) (string, error) {
	d := tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}} // judged by the PAKE
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("%s unreachable: %w", addr, audio.DialError(addr, err))
	}
	defer c.Close()
	certs := c.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("%s presented no certificate", addr)
	}
	return audio.CertFingerprint(certs[0].Raw), nil
}

func post(ctx context.Context, hc *http.Client, addr, path string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable: %w", addr, audio.DialError(addr, err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK:
		return json.Unmarshal(body, out)
	case http.StatusForbidden:
		return ErrRefused
	case http.StatusConflict:
		return ErrNameTaken
	}
	return fmt.Errorf("pairing: %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

// pairStart runs the box's half of the PAKE for a live PIN. Every run is one
// of the PIN's tries, counted before the box answers, since the answer lets
// the caller test one guess.
func (s *Server) pairStart(w http.ResponseWriter, r *http.Request) {
	var req startReq
	if err := readJSON(w, r, &req); err != nil {
		return
	}
	if !nameRE.MatchString(req.Name) {
		http.Error(w, "name: letters, digits, '.', '_' and '-', up to 64", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pw := s.livePIN()
	if pw == nil || pw.tries >= PINTries {
		s.logf("pairing %s from %s refused: no live PIN", req.Name, r.RemoteAddr)
		http.Error(w, ErrRefused.Error(), http.StatusForbidden)
		return
	}
	if s.client(req.Name) != nil {
		s.logf("pairing %s from %s refused: name taken", req.Name, r.RemoteAddr)
		http.Error(w, ErrNameTaken.Error(), http.StatusConflict)
		return
	}
	pw.tries++
	displaced := ""
	if pw.pending != nil {
		displaced = "; displaces " + pw.pending.name + "'s pending pairing"
	}
	s.logf("pairing %s from %s: try %d of %d%s", req.Name, r.RemoteAddr, pw.tries, PINTries, displaced)
	pw.pending = nil
	msgB, resp, err := respondPAKE(pw.code, req.Name, adFor(s.fp), req.A)
	if err != nil {
		s.spent(pw, req.Name, r.RemoteAddr, err.Error())
		http.Error(w, "bad pairing message", http.StatusBadRequest)
		return
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	pw.pending = &pendingPair{id: hex.EncodeToString(id), name: req.Name, r: resp}
	writeJSON(w, startResp{ID: pw.pending.id, B: msgB})
}

// pairConfirm checks the Mac's key confirmation; a match consumes the PIN,
// pairs the name and answers with the token sealed under the session.
func (s *Server) pairConfirm(w http.ResponseWriter, r *http.Request) {
	var req confirmReq
	if err := readJSON(w, r, &req); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pw := s.livePIN()
	if pw == nil || pw.pending == nil || subtle.ConstantTimeCompare([]byte(req.ID), []byte(pw.pending.id)) != 1 {
		s.logf("pairing confirmation from %s refused: nothing pending", r.RemoteAddr)
		http.Error(w, ErrRefused.Error(), http.StatusForbidden)
		return
	}
	p := pw.pending
	pw.pending = nil
	key, err := p.r.finish(req.Confirm)
	if err != nil {
		s.spent(pw, p.name, r.RemoteAddr, "wrong PIN or someone in the path")
		http.Error(w, ErrRefused.Error(), http.StatusForbidden)
		return
	}
	s.pinw = nil
	token, err := s.addClient(p.name)
	if errors.Is(err, ErrNameTaken) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	} else if err != nil {
		s.logf("pairing %s: %v", p.name, err)
		http.Error(w, "pairing failed on the box", http.StatusInternalServerError)
		return
	}
	sealed, tag := boxSeal(key, p.r.msgA, p.r.msgB, s.cfg.Name, p.name, token)
	s.logf("paired %s from %s", p.name, r.RemoteAddr)
	writeJSON(w, confirmResp{Box: s.cfg.Name, Token: sealed, Confirm: tag})
}

// spent logs a failed try and ends the PIN when it has none left; the
// caller holds s.mu.
func (s *Server) spent(pw *pinWindow, name, addr, why string) {
	left := PINTries - pw.tries
	s.logf("pairing %s from %s refused: %s; %d tries left", name, addr, why, left)
	if left <= 0 {
		s.pinw = nil
	}
}

// boxSeal seals token under the session and tags the answer: the Mac takes
// the token only from the box that confirmed the same PIN and certificate.
func boxSeal(key, msgA, msgB []byte, box, name, token string) (sealed, tag []byte) {
	g := gcm(derive(key, "token"))
	nonce := make([]byte, g.NonceSize())
	_, _ = rand.Read(nonce)
	sealed = g.Seal(nonce, nonce, []byte(token), []byte(name))
	return sealed, macOf(derive(key, "confirm box"), msgA, msgB, []byte(box), []byte(name), sealed)
}

func macOpen(key, msgA, msgB []byte, box, name string, sealed, tag []byte) (string, error) {
	if !hmac.Equal(tag, macOf(derive(key, "confirm box"), msgA, msgB, []byte(box), []byte(name), sealed)) {
		return "", errConfirm
	}
	g := gcm(derive(key, "token"))
	if len(sealed) < g.NonceSize() {
		return "", errConfirm
	}
	tok, err := g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], []byte(name))
	if err != nil {
		return "", errConfirm
	}
	return string(tok), nil
}

func gcm(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // derive's 32 bytes are an AES-256 key
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(v)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
	}
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
