package audio

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
)

// ErrNotPinned is a TLS handshake's failure when the server's certificate
// is not the one pinned.
var ErrNotPinned = errors.New("certificate is not the paired one")

// CertFingerprint is the SHA-256 of a certificate's DER, lowercase hex: what
// a paired remote pins.
func CertFingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

// PinnedTLS trusts exactly the server certificate whose fingerprint is fp:
// no CA, no name, no expiry, since pairing confirmed that certificate.
func PinnedTLS(fp string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyConnection's pin
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrNotPinned
			}
			got := CertFingerprint(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(got), []byte(fp)) != 1 {
				return ErrNotPinned
			}
			return nil
		},
	}
}
