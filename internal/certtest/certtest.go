// Package certtest mints self-signed certificates for tests that need a TLS
// server with a known, trusted-by-the-test certificate.
//
// It takes a *testing.T rather than returning an error, and it imports
// "testing" despite not being a _test.go file — both unusual for a
// non-test package. That is deliberate: this package exists only to be
// called from tests, and t.Fatalf on a minting failure is what every
// caller would otherwise do by hand with the error return.
package certtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// For mints a self-signed certificate valid for name, and the pool that
// trusts it. Returned separately so a test can hand the pool to one
// exchanger and deliberately not to another.
func For(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	return ForWithExpiry(t, name, time.Now().Add(time.Hour))
}

// ForWithExpiry is For with an explicit NotAfter, so a test can mint a
// certificate that is already close to expiry.
func ForWithExpiry(t *testing.T, name string, notAfter time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the certificate back: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// EncodeCert returns the PEM encoding of cert's leaf, as it would appear in
// certbot's fullchain.pem — the form CertProvider reads from disk.
func EncodeCert(cert tls.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
}

// EncodeKey returns the PEM encoding of cert's private key, as it would
// appear in certbot's privkey.pem.
func EncodeKey(t *testing.T, cert tls.Certificate) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatalf("marshaling the private key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
