package micserver

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// client is a paired Mac as clients.toml keeps it: the token's SHA-256,
// never the token.
type client struct {
	Name      string    `toml:"name"`
	TokenHash string    `toml:"token_hash"`
	Paired    time.Time `toml:"paired"`
	LastSeen  time.Time `toml:"last_seen"`
}

type clientsFile struct {
	Client []*client `toml:"client"`
}

// nameRE is a client name: what `mic revoke NAME` takes on the box.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ErrNameTaken is the refusal of a client name that is already paired.
var ErrNameTaken = errors.New("a client by that name is already paired")

// loadCert reads the key and certificate from dir, creating a P-256 key
// (0600) and a self-signed certificate for name when there is no key.
func loadCert(dir, name string) (tls.Certificate, error) {
	keyPath, certPath := filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")
	keyPEM, err := os.ReadFile(keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return tls.Certificate{}, err
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := writeFile(keyPath, keyPEM, 0o600); err != nil {
			return tls.Certificate{}, err
		}
		_ = os.Remove(certPath) // a certificate of another key
	} else if err != nil {
		return tls.Certificate{}, err
	}
	certPEM, err := os.ReadFile(certPath)
	if errors.Is(err, fs.ErrNotExist) {
		if certPEM, err = selfSign(keyPEM, name); err != nil {
			return tls.Certificate{}, err
		}
		if err := writeFile(certPath, certPEM, 0o644); err != nil {
			return tls.Certificate{}, err
		}
	} else if err != nil {
		return tls.Certificate{}, err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s, %s: %w", keyPath, certPath, err)
	}
	return c, nil
}

func selfSign(keyPEM []byte, name string) ([]byte, error) {
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, errors.New("key.pem: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("key.pem: not an ECDSA key")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "megavoice mic " + name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(100, 0, 0), // pinned, never checked against a CA or a clock
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &signer.PublicKey, signer)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func loadClients(path string) ([]*client, error) {
	var f clientsFile
	if _, err := toml.DecodeFile(path, &f); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return f.Client, nil
}

// save writes clients.toml; the caller holds s.mu.
func (s *Server) save() error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(clientsFile{Client: s.clients}); err != nil {
		return err
	}
	return writeFile(filepath.Join(s.cfg.State, "clients.toml"), buf.Bytes(), 0o600)
}

// writeFile replaces path with b at mode, never leaving a partial file.
func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// addClient pairs name with a fresh token and returns the token; the caller
// holds s.mu when called from a handler.
func (s *Server) addClient(name string) (string, error) {
	if s.client(name) != nil {
		return "", ErrNameTaken
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	prev := s.clients
	s.clients = append(slices.Clone(prev), &client{Name: name, TokenHash: tokenHash(token), Paired: s.now().UTC()})
	slices.SortFunc(s.clients, func(a, b *client) int { return strings.Compare(a.Name, b.Name) })
	if err := s.save(); err != nil {
		s.clients = prev // a pairing the box would forget at restart is none
		return "", err
	}
	return token, nil
}

func (s *Server) client(name string) *client {
	for _, c := range s.clients {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// byToken is the client whose token hashes to token's hash, compared in
// constant time.
func (s *Server) byToken(token string) *client {
	h, _ := hex.DecodeString(tokenHash(token))
	var found *client
	for _, c := range s.clients {
		ch, _ := hex.DecodeString(c.TokenHash)
		if subtle.ConstantTimeCompare(h, ch) == 1 {
			found = c
		}
	}
	return found
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
