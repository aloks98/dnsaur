package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/miekg/dns"
)

const dohContentType = "application/dns-message"

// dohExchanger is DNS over HTTPS (RFC 8484), POST only.
type dohExchanger struct {
	url    string
	client *http.Client
}

// newDoHExchanger builds the transport for u.
//
// The URL is built from u.VerifyName and the dialer is pinned to u.Addr.
// That inversion is what lets a DNS server use DNS-over-HTTPS without
// needing DNS: the name in the URL makes the Host header and the TLS SNI
// correct with no special-casing, and DialContext discards the address it
// is handed, so net/http never performs a lookup.
func newDoHExchanger(u Upstream, timeout time.Duration, roots *x509.CertPool) *dohExchanger {
	dialer := &net.Dialer{Timeout: timeout}
	// The URL keeps a non-default port so the Host header matches what the
	// operator configured; on 443 it is omitted, as it would be in a browser.
	host := u.VerifyName
	if _, port, err := net.SplitHostPort(u.Addr); err == nil && port != defaultDoHPort {
		host = net.JoinHostPort(u.VerifyName, port)
	}
	return &dohExchanger{
		// Concatenated rather than assembled through url.URL: u.Path is
		// already the escaped path (addr.go uses EscapedPath), and url.URL's
		// String() re-escapes whatever is in its Path field -- a configured
		// "%2F" would come back out as "%252F". host is a DNS name plus an
		// optional numeric port, so it needs no escaping either.
		url: "https://" + host + u.Path,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				ForceAttemptHTTP2:   true,
				MaxIdleConnsPerHost: maxIdleConns,
				IdleConnTimeout:     idleConnTimeout,
				TLSClientConfig: &tls.Config{
					RootCAs:    roots, // nil = the system roots
					MinVersion: tls.VersionTLS12,
				},
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					// The address argument is discarded on purpose: it was
					// derived from the URL's host, which is a name we must
					// never look up.
					return dialer.DialContext(ctx, network, u.Addr)
				},
			},
		},
	}
}

func (e *dohExchanger) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	if err := padQuery(m, paddingBlock); err != nil {
		return nil, err
	}
	// RFC 8484 §4.1: the ID carries no meaning over HTTP, and a constant
	// zero makes responses cacheable by the HTTP layer. The caller's ID is
	// restored below, because callers above this seam do match on it.
	id := m.Id
	m.Id = 0
	wire, err := m.Pack()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", dohContentType)
	req.Header.Set("Accept", dohContentType)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drained before the deferred Close: http.Transport only returns a
		// connection to the idle pool once its body has been read to EOF, so
		// closing an unread error body costs the connection. Bounded, because
		// an upstream answering 500 with a gigabyte of HTML is not something
		// to read in full.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, dns.MaxMsgSize))
		return nil, fmt.Errorf("doh %s: %s", e.url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dns.MaxMsgSize))
	if err != nil {
		return nil, err
	}
	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		return nil, fmt.Errorf("doh %s: %w", e.url, err)
	}
	r.Id = id
	stripPadding(r)
	return r, nil
}

// Close releases the transport's idle connections.
func (e *dohExchanger) Close() error {
	e.client.CloseIdleConnections()
	return nil
}
