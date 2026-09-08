package dnssrv

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// CertProvider serves a TLS keypair read from disk, reloading it whenever
// either file's modification time moves.
//
// certbot (or rsync, or a manual copy) replaces the certificate and key in
// place every 60-90 days. Reading them once at startup would mean renewal
// silently stops working: dnsaur keeps serving the certificate it loaded at
// boot until the process is restarted, and DNS fails three months later for
// a reason that looks nothing like a certificate. Comparing mtime on every
// handshake and reloading only when it has moved costs one Stat per file
// per handshake and stays correct without a watcher or a timer.
type CertProvider struct {
	certPath string
	keyPath  string

	mu        sync.Mutex
	cert      *tls.Certificate
	certModAt time.Time
	keyModAt  time.Time
}

// NewCertProvider builds a provider that reads certPath and keyPath. Nothing
// is read until the first call to GetCertificate.
func NewCertProvider(certPath, keyPath string) *CertProvider {
	return &CertProvider{certPath: certPath, keyPath: keyPath}
}

// GetCertificate satisfies tls.Config.GetCertificate. It serves the cached
// keypair unless either file's mtime has moved since the last load, in
// which case it reloads first.
//
// Stat and read happen outside p.mu — held only long enough to swap the
// cached value in — so a slow or hung filesystem stalls the handshake doing
// the reload, not every other concurrent handshake waiting on the lock.
func (p *CertProvider) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	// os.Stat's own error is already "stat <path>: <cause>" (an *fs.PathError,
	// whose Op is "stat"): wrapping it with another "stat %s:" would repeat
	// both the operation and the path back at the reader. "certificate"/"key"
	// says which half failed without saying anything the wrapped error
	// already says.
	certInfo, err := os.Stat(p.certPath)
	if err != nil {
		return p.cached(fmt.Errorf("certificate: %w", err))
	}
	keyInfo, err := os.Stat(p.keyPath)
	if err != nil {
		return p.cached(fmt.Errorf("key: %w", err))
	}

	p.mu.Lock()
	unchanged := p.cert != nil && certInfo.ModTime().Equal(p.certModAt) && keyInfo.ModTime().Equal(p.keyModAt)
	cached := p.cert
	p.mu.Unlock()
	if unchanged {
		return cached, nil
	}

	cert, err := tls.LoadX509KeyPair(p.certPath, p.keyPath)
	if err != nil {
		// The files may be mid-write — certbot's replacement is not atomic
		// on every platform. Report the error for this handshake only; a
		// cached keypair from a previous successful load, if any, is left
		// in place rather than discarded, so a transient read error does
		// not turn into an outage that persists until the next successful
		// write.
		return p.cached(fmt.Errorf("loading keypair: %w", err))
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return p.cached(fmt.Errorf("parsing certificate: %w", err))
	}
	cert.Leaf = leaf

	p.mu.Lock()
	p.cert = &cert
	p.certModAt = certInfo.ModTime()
	p.keyModAt = keyInfo.ModTime()
	p.mu.Unlock()

	return &cert, nil
}

// cached returns the last successfully loaded keypair, if there is one, and
// otherwise err. It never discards p.cert: a failed reload is reported for
// this handshake alone.
func (p *CertProvider) cached(err error) (*tls.Certificate, error) {
	p.mu.Lock()
	cert := p.cert
	p.mu.Unlock()
	if cert != nil {
		return cert, nil
	}
	return nil, err
}

// Leaf returns the currently loaded certificate for inspection — the
// subject name and NotAfter the Settings screen shows, and the expiry
// warning checks. It returns nil until GetCertificate has succeeded at
// least once.
func (p *CertProvider) Leaf() *x509.Certificate {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cert == nil {
		return nil
	}
	return p.cert.Leaf
}
