// Package dhcp is dnsaur's side of the DHCP engine (design §1): kea-dhcp4
// serves the protocol, dnsaur renders its configuration and reads leases and
// status back over the engine's unix control socket (§5, §7).
package dhcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Kea closes the connection after one reply, so every command is its own
// dial, and these are the deadlines that dial-write-read gets when the
// caller's context carries none. config-set is the slow one: Kea rebuilds
// every subnet and re-reads its lease file before it answers.
const (
	defaultTimeout   = 5 * time.Second
	configSetTimeout = 30 * time.Second
)

// resultEmpty is Kea's "nothing found". lease4-get-page answers with it for
// the page past the last lease, which ends pagination rather than failing.
const resultEmpty = 3

// ErrUnreachable wraps every failure to reach the control socket, so callers
// can tell "the engine is not there" (design §7.1: keep the lease table,
// report the engine unreachable) from "the engine said no".
var ErrUnreachable = errors.New("engine unreachable")

// RejectedError is a command Kea answered with a non-zero result. Text is
// Kea's message unchanged, because that is what the operator is shown
// (design §5.1).
type RejectedError struct{ Text string }

func (e *RejectedError) Error() string { return "the engine refused the command: " + e.Text }

// Response is one Kea reply. Arguments stays raw: each command decodes its
// own shape, and config-get's is the whole server configuration.
type Response struct {
	Result    int             `json:"result"`
	Text      string          `json:"text"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Lease is one entry of Kea's lease database, tagged with the keys Kea uses
// so it decodes straight out of a lease4-get-page reply.
type Lease struct {
	IP       string `json:"ip-address"`
	MAC      string `json:"hw-address"`
	Hostname string `json:"hostname"`
	SubnetID int64  `json:"subnet-id"`
	CLTT     int64  `json:"cltt"`
	ValidLft int64  `json:"valid-lft"`
	State    int    `json:"state"`
}

// HAStatus is the part of status-get the dashboard shows (design §8.4).
type HAStatus struct {
	Mode       string
	LocalState string
	// RemoteName is what the partner calls itself — the name this box's own
	// configuration gave it, echoed back by the engine that is talking to
	// it. The dashboard's status line is "hot-standby with <peer>", and the
	// alternative was for dnsaur to re-derive the name from the pair it
	// rendered, which would say who it *meant* to be talking to.
	//
	// Empty on an engine whose status-get does not carry it, which is the
	// same thing the line has to handle for an engine with no HA hook at
	// all.
	RemoteName               string
	RemoteState              string
	CommunicationInterrupted bool
	UnackedClients           int
}

// Client talks to one kea-dhcp4 control socket.
type Client struct {
	socket string
}

// NewClient returns a client for the engine listening on socket.
func NewClient(socket string) *Client { return &Client{socket: socket} }

// Command sends one command and returns Kea's reply. A non-zero result is
// not an error here: some commands read meaning into the code, so the caller
// decides. Dial failures are wrapped in ErrUnreachable.
func (c *Client) Command(ctx context.Context, name string, args any) (Response, error) {
	return c.command(ctx, defaultTimeout, name, args)
}

func (c *Client) command(ctx context.Context, timeout time.Duration, name string, args any) (Response, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %s: %w", ErrUnreachable, c.socket, err)
	}
	defer func() { _ = conn.Close() }()

	// One timer covers the whole exchange: the context's own deadline, and a
	// cancellation from the caller, both land on the connection.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	req := struct {
		Command   string `json:"command"`
		Arguments any    `json:"arguments,omitempty"`
	}{Command: name, Arguments: args}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, failed(ctx, "sending "+name, err)
	}
	// Kea answers once and closes, so the reply is everything up to EOF.
	body, err := io.ReadAll(conn)
	if err != nil {
		return Response{}, failed(ctx, "reading the reply to "+name, err)
	}
	if len(body) == 0 {
		return Response{}, failed(ctx, "reading the reply to "+name, errors.New("the engine closed the connection without answering"))
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return Response{}, fmt.Errorf("decoding the reply to %s: %w", name, err)
	}
	return resp, nil
}

// failed names an exchange that broke after the dial. A context error is the
// caller's own deadline or cancellation and stays itself; anything else is
// the engine going away mid-command — a restart, a reset, a connection
// closed without an answer — which is ErrUnreachable just as much as a
// socket that would not dial.
func failed(ctx context.Context, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", what, ctxErr)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnreachable, what, err)
}

// Version returns the engine's version ("2.6.3"). The renderer needs it to
// decide which keys this Kea understands (design §6).
func (c *Client) Version(ctx context.Context) (string, error) {
	resp, err := c.Command(ctx, "version-get", nil)
	if err != nil {
		return "", err
	}
	if err := rejected(resp); err != nil {
		return "", err
	}
	version := resp.Text
	if version == "" && len(resp.Arguments) > 0 {
		var args struct {
			Extended string `json:"extended"`
		}
		if err := json.Unmarshal(resp.Arguments, &args); err != nil {
			return "", fmt.Errorf("decoding version-get arguments: %w", err)
		}
		version = args.Extended
	}
	// extended carries the build after the version; text does not.
	version, _, _ = strings.Cut(strings.TrimSpace(version), " ")
	return version, nil
}

// ConfigGet returns the engine's running configuration, the object with the
// Dhcp4 key in it. The renderer reads the hook directory out of it (§5.2).
func (c *Client) ConfigGet(ctx context.Context) (map[string]any, error) {
	resp, err := c.Command(ctx, "config-get", nil)
	if err != nil {
		return nil, err
	}
	if err := rejected(resp); err != nil {
		return nil, err
	}
	var cfg map[string]any
	if len(resp.Arguments) > 0 {
		if err := json.Unmarshal(resp.Arguments, &cfg); err != nil {
			return nil, fmt.Errorf("decoding config-get arguments: %w", err)
		}
	}
	return cfg, nil
}

// ConfigSet applies dhcp4 as the engine's whole Dhcp4 object. A refused
// config leaves Kea on its previous one and comes back as a *RejectedError
// carrying Kea's message (design §5.1).
func (c *Client) ConfigSet(ctx context.Context, dhcp4 map[string]any) error {
	resp, err := c.command(ctx, configSetTimeout, "config-set", map[string]any{"Dhcp4": dhcp4})
	if err != nil {
		return err
	}
	return rejected(resp)
}

// LeasePage returns one page of leases and the "from" for the next call, or
// "" when this was the last page. Pass "" as from to start.
func (c *Client) LeasePage(ctx context.Context, from string, limit int) ([]Lease, string, error) {
	if from == "" {
		from = "start"
	}
	resp, err := c.Command(ctx, "lease4-get-page", map[string]any{"from": from, "limit": limit})
	if err != nil {
		return nil, "", err
	}
	if resp.Result != 0 && resp.Result != resultEmpty {
		return nil, "", &RejectedError{Text: resp.Text}
	}
	var page struct {
		Leases []Lease `json:"leases"`
	}
	if len(resp.Arguments) > 0 {
		if err := json.Unmarshal(resp.Arguments, &page); err != nil {
			return nil, "", fmt.Errorf("decoding lease4-get-page arguments: %w", err)
		}
	}
	// A short page is the last one; a full one is followed by another,
	// starting after the address this page ended on. What was decoded
	// decides, so the reply's count is never read.
	next := ""
	if len(page.Leases) >= limit {
		next = page.Leases[len(page.Leases)-1].IP
	}
	return page.Leases, next, nil
}

// DeleteLease releases one address. A lease Kea does not hold comes back as
// a *RejectedError with Kea's text.
func (c *Client) DeleteLease(ctx context.Context, ip string) error {
	resp, err := c.Command(ctx, "lease4-del", map[string]any{"ip-address": ip})
	if err != nil {
		return err
	}
	return rejected(resp)
}

// Status returns the engine's uptime in seconds and its HA state, which is
// nil when the HA hook is not loaded (a single box, design §6).
func (c *Client) Status(ctx context.Context) (int64, *HAStatus, error) {
	resp, err := c.Command(ctx, "status-get", nil)
	if err != nil {
		return 0, nil, err
	}
	if err := rejected(resp); err != nil {
		return 0, nil, err
	}
	var args struct {
		Uptime int64 `json:"uptime"`
		HA     []struct {
			Mode    string `json:"ha-mode"`
			Servers struct {
				Local struct {
					State string `json:"state"`
				} `json:"local"`
				Remote struct {
					ServerName               string `json:"server-name"`
					LastState                string `json:"last-state"`
					CommunicationInterrupted bool   `json:"communication-interrupted"`
					UnackedClients           int    `json:"unacked-clients"`
				} `json:"remote"`
			} `json:"ha-servers"`
		} `json:"high-availability"`
	}
	if len(resp.Arguments) > 0 {
		if err := json.Unmarshal(resp.Arguments, &args); err != nil {
			return 0, nil, fmt.Errorf("decoding status-get arguments: %w", err)
		}
	}
	if len(args.HA) == 0 {
		return args.Uptime, nil, nil
	}
	// Kea reports one entry per HA relationship; dnsaur renders a single
	// hot-standby pair (design §6), so the first is the only one.
	ha := args.HA[0]
	return args.Uptime, &HAStatus{
		Mode:                     ha.Mode,
		LocalState:               ha.Servers.Local.State,
		RemoteName:               ha.Servers.Remote.ServerName,
		RemoteState:              ha.Servers.Remote.LastState,
		CommunicationInterrupted: ha.Servers.Remote.CommunicationInterrupted,
		UnackedClients:           ha.Servers.Remote.UnackedClients,
	}, nil
}

func rejected(resp Response) error {
	if resp.Result == 0 {
		return nil
	}
	return &RejectedError{Text: resp.Text}
}
