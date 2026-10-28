package remote

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/caoer/mega-asr/internal/relay/wire"
)

// phone is a paired phone as phones.toml keeps it: its token's SHA-256,
// never the token, and the relay's public keys of the phone and its watch
// (base64url), registered as PROTOCOL.md § 8 allows.
type phone struct {
	Name       string    `toml:"name"`
	TokenHash  string    `toml:"token_hash"`
	Paired     time.Time `toml:"paired"`
	LastSeen   time.Time `toml:"last_seen"`
	SigPK      string    `toml:"sig_pk,omitempty"`
	WatchSigPK string    `toml:"watch_sig_pk,omitempty"`
}

type phonesFile struct {
	Phone []*phone `toml:"phone"`
}

// ErrUnknownPhone is Revoke's error for a name that is not paired.
var ErrUnknownPhone = errors.New("no paired phone by that name")

// deviceName checks a phone's name: 1 to 64 characters, no control
// character, trimmed.
func deviceName(s string) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n == 0 || n > 64 || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return "", fmt.Errorf("device name %q: 1 to 64 characters, no control character", s)
	}
	return s, nil
}

func loadPhones(path string) ([]*phone, error) {
	var f phonesFile
	if _, err := toml.DecodeFile(path, &f); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return f.Phone, nil
}

// save writes phones.toml; the caller holds s.mu.
func (s *Server) save() error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(phonesFile{Phone: s.phones}); err != nil {
		return err
	}
	return writeFile(filepath.Join(s.cfg.State, "phones.toml"), buf.Bytes(), 0o600)
}

// writeFile replaces path with b at mode, never leaving a partial file.
func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp, err := stage(path, b, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

// stage writes b at mode to a new file beside path and returns its name;
// renaming it onto path replaces path whole.
func stage(path string, b []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(b)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// addPhone pairs name with a fresh token and returns it; a phone of that
// name is replaced, its token refused from then on and its connections
// closed. The caller holds s.mu.
func (s *Server) addPhone(name string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	old := s.phone(name)
	prev := s.phones
	s.phones = slices.DeleteFunc(slices.Clone(prev), func(p *phone) bool { return p == old })
	s.phones = append(s.phones, &phone{Name: name, TokenHash: tokenHash(token), Paired: s.now().UTC()})
	slices.SortFunc(s.phones, func(a, b *phone) int { return strings.Compare(a.Name, b.Name) })
	if err := s.save(); err != nil {
		s.phones = prev // a pairing the Mac would forget at restart is none
		return "", err
	}
	if old != nil {
		s.closeConns(old)
	}
	return token, nil
}

func (s *Server) phone(name string) *phone {
	for _, p := range s.phones {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// byToken is the phone whose token hashes to token's hash, compared in
// constant time; the caller holds s.mu.
func (s *Server) byToken(token string) *phone {
	if token == "" {
		return nil
	}
	h, _ := hex.DecodeString(tokenHash(token))
	var found *phone
	for _, p := range s.phones {
		ph, _ := hex.DecodeString(p.TokenHash)
		if subtle.ConstantTimeCompare(h, ph) == 1 {
			found = p
		}
	}
	return found
}

// byKey is the phone whose relay key — the phone's own, or its watch's when
// watch — is key, compared in constant time; the caller holds s.mu.
func (s *Server) byKey(key []byte, watch bool) *phone {
	if len(key) != ed25519.PublicKeySize {
		return nil
	}
	var found *phone
	for _, p := range s.phones {
		k := p.SigPK
		if watch {
			k = p.WatchSigPK
		}
		if pk, err := wire.Unb64(k, ed25519.PublicKeySize); err == nil && subtle.ConstantTimeCompare(pk, key) == 1 {
			found = p
		}
	}
	return found
}

// keyTaken reports whether key is already some device's relay key other
// than p's in the role given; the caller holds s.mu.
func (s *Server) keyTaken(key []byte, p *phone, watch bool) bool {
	k := wire.B64(key)
	for _, q := range s.phones {
		if (q.SigPK == k && (q != p || watch)) || (q.WatchSigPK == k && (q != p || !watch)) {
			return true
		}
	}
	return false
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
