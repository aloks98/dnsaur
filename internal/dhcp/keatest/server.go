// Package keatest runs a fake kea-dhcp4 control socket for tests: a real
// unix listener speaking Kea's one-request-one-reply protocol, so the client
// under test takes the same dial, encode and read-to-EOF path it takes
// against the real engine.
//
// Like certtest it takes a *testing.T and imports "testing" from a non-test
// file. Every caller is a test, and t.Fatalf on a failure to listen is what
// each of them would otherwise write by hand.
package keatest

import (
	"encoding/json"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/aloks98/dnsaur/internal/dhcp"
)

// Server is a fake engine listening on a unix socket in the test's temporary
// directory. It answers registered commands from Handle and everything else
// the way Kea answers an unknown command; it is closed when the test ends.
type Server struct {
	socket string
	ln     net.Listener
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	handlers map[string]func(args json.RawMessage) dhcp.Response
	calls    []string
	hang     bool
	drop     bool
}

// NewServer starts a server on a socket under t.TempDir().
func NewServer(t *testing.T) *Server {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "kea.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}
	s := &Server{
		socket:   socket,
		ln:       ln,
		done:     make(chan struct{}),
		handlers: map[string]func(json.RawMessage) dhcp.Response{},
	}
	t.Cleanup(s.Close)
	go s.serve()
	return s
}

// Socket is the path to hand NewClient.
func (s *Server) Socket() string { return s.socket }

// Handle makes fn the answer to command, replacing any earlier one. fn runs
// on the connection's goroutine, so anything it reports to the test has to
// travel over a channel rather than a plain variable.
func (s *Server) Handle(command string, fn func(args json.RawMessage) dhcp.Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[command] = fn
}

// Hang makes every connection from here on read its request and never
// answer, until Close. A caller then sees nothing but its own deadline.
func (s *Server) Hang() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hang = true
}

// Drop makes every connection from here on read its request and close
// without answering, the way an engine restarting mid-command does.
func (s *Server) Drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop = true
}

// Calls returns the command names the server has been asked for, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Close stops the listener and releases any hung connection. Calling it
// twice is safe; the test's cleanup calls it once.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.ln.Close()
	})
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // Close closed the listener.
		}
		go s.answer(conn)
	}
}

func (s *Server) answer(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var req struct {
		Command   string          `json:"command"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, req.Command)
	fn, known := s.handlers[req.Command]
	hang, drop := s.hang, s.drop
	s.mu.Unlock()

	if hang {
		<-s.done
		return
	}
	if drop {
		return // the deferred Close ends the connection unanswered
	}
	resp := dhcp.Response{Result: 2, Text: "'" + req.Command + "' command not supported."}
	if known {
		resp = fn(req.Arguments)
	}
	_ = json.NewEncoder(conn).Encode(resp)
}
