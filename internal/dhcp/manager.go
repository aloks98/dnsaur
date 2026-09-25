package dhcp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

// The three states §8.3 reports the engine in, and nothing else: "ok" is a
// config the engine took and a poll that reached it, and the other two are
// the only ways that stops being true.
const (
	engineOK       = "ok"
	engineDown     = "unreachable"
	engineRejected = "config rejected"
	pollSetting    = "dhcp.lease_poll_seconds"
)

const (
	// leasePageLimit is how many leases one lease4-get-page carries. A home
	// LAN is one page; the poller keeps asking until a page comes back short.
	leasePageLimit = 500
	// defaultPollSeconds and minPollSeconds bracket dhcp.lease_poll_seconds
	// (§4.2). A setting that cannot be read falls back to the default; one
	// below the floor is raised to it, because a poll per second is a
	// hundred commands a minute at the engine for no new information.
	defaultPollSeconds = 10
	minPollSeconds     = 2
)

// hookDirs are the two packaged hook directories, Debian's first (§5.2).
// They are only consulted when the engine's own configuration names none,
// which is an engine started with no hooks at all.
var hookDirs = []string{"/usr/lib/x86_64-linux-gnu/kea/hooks", "/usr/lib/kea/hooks"}

// Inputs is everything the manager needs from the rest of dnsaur to render a
// configuration: the scopes and reservations, the settings behind them, and
// this box's place in the pair. It is one method because it is one question,
// asked before every render and every poll, and the app is what knows how to
// answer it.
type Inputs interface {
	RenderInput(ctx context.Context) (RenderInput, error)
	// Rendered says what became of one render: in as this method's other
	// half supplied it — the engine's own two facts are not in it — and err
	// nil once the engine has taken that configuration.
	//
	// It exists because two things in the app hang off a render *being
	// accepted* rather than off it being attempted: the HA pair a main
	// publishes for its standby to read (§6), and the record of what the
	// engine is holding that §5.1's gate compares against. Every path that
	// renders goes through here, so neither can be left behind by one of
	// them.
	Rendered(ctx context.Context, in RenderInput, err error)
}

// ScopeUsage is how full one scope's pool is (§8.3).
type ScopeUsage struct {
	ID       int64
	PoolSize int
	Leased   int
}

// Status is the whole of what dnsaur knows about the engine (§8.3): whether
// DHCP runs at all, what state the engine is in and why, how old the lease
// table is, and the HA relationship when there is one.
type Status struct {
	Enabled         bool
	Engine          string
	EngineVersion   string
	Message         string
	TableAgeSeconds int64
	HA              *HAStatus
	Scopes          []ScopeUsage
}

// Manager is dnsaur's half of the engine: it renders and applies the
// configuration, polls the leases into a table readers share, and keeps the
// one-line answer to "is DHCP working".
type Manager struct {
	client   *Client
	inputs   Inputs
	settings store.SettingsStore
	log      *slog.Logger

	// polling is the poll loop's own lifetime, so a caller that cancels the
	// context can wait for it — see Wait.
	polling sync.WaitGroup

	// applyMu serialises renders. Two of them racing would have the engine
	// end on whichever config-set landed second, which is not necessarily
	// the newer configuration.
	applyMu sync.Mutex

	table atomic.Pointer[Table]
	// tableMu serialises everything that replaces the table. Loading it is
	// one atomic read and needs nothing, but Release is a read-modify-write
	// over the same pointer a poll swaps: without this, two releases at once
	// each read the table the other was about to replace and the second one
	// stored put the first one's row back — answering DNS with a name for an
	// address the engine had already taken back.
	tableMu sync.Mutex

	mu   sync.Mutex
	subs []func(*Table)
	// The three things that can be wrong with the engine, each with one owner
	// and one way out, because they are independent: it may be unreachable, it
	// may be running a configuration it would not replace, and it may have
	// refused a command on the last poll. Collapsing them lets one clear
	// another, and "ok" on the dashboard would then mean "the last thing that
	// happened went well" rather than "dnsaur's configuration is in force".
	down    bool
	downErr string
	// returned is an engine that has answered since it was last unreachable,
	// and has not been re-discovered and re-rendered for it yet (§10). It is
	// a flag rather than a "was it down" return value because the exchange
	// that finds the engine again is not always the one that can act on it:
	// a poll whose lease4-get-page comes back refused has still reached the
	// engine, and the re-render is owed to the next poll that can.
	returned     bool
	applyErr     string
	pollErr      string
	version      string
	hookDir      string
	engineSocket string
	ha           *HAStatus
	// hash identifies the configuration the engine was last given, accepted
	// or refused, and is what ApplyIfChanged compares against. Empty until
	// something has reached the engine, which compares equal to nothing.
	hash string
	// The last inputs seen, kept so Status can size the pools and a poll can
	// mark reserved leases without asking the app again when it cannot.
	scopes       []store.Scope
	reservations []store.Reservation
	classes      []store.Class
	domain       string
}

// NewManager wires a manager to one engine. logger may be nil, which is the
// default logger.
func NewManager(client *Client, inputs Inputs, settings store.SettingsStore, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		client:   client,
		inputs:   inputs,
		settings: settings,
		log:      logger,
		// Nothing has answered yet, which is the honest state to start in:
		// the first exchange of Start replaces it either way.
		down:    true,
		hookDir: hookDirs[0],
	}
	m.table.Store(newTable(nil))
	return m
}

// Start asks the engine what it is, renders once, and leaves a poller running
// until ctx ends. It returns after the first poll, so a caller that starts the
// DNS side next finds whatever leases the engine already held.
//
// Nothing here is fatal (§10): an engine that is not there and a config it
// refuses are both states the operator is shown and the next change clears,
// not reasons for dnsaur to refuse to run. The error is reserved for a
// failure that would be.
func (m *Manager) Start(ctx context.Context) error {
	m.discover(ctx)
	if err := m.Apply(ctx); err != nil {
		m.log.Warn("applying the dhcp configuration failed", "err", err)
	}
	if err := m.Poll(ctx); err != nil {
		m.log.Warn("the first dhcp lease poll failed", "err", err)
	}
	m.polling.Add(1)
	go func() {
		defer m.polling.Done()
		m.loop(ctx)
	}()
	return nil
}

// Wait blocks until the poll loop has stopped, which it does when the
// context Start was given ends. It is what lets a caller close the store the
// poller reads its scopes from: without it, a shutdown races a poll that is
// already inside RenderInput, and the process spends its last moments
// logging that the database it closed will not answer.
func (m *Manager) Wait() { m.polling.Wait() }

// discover reads the two facts the renderer cannot know without asking: the
// engine's version, which decides the keys it understands, and where its hook
// libraries live (§5.2). Neither is fatal — an engine that answers nothing
// gets a config rendered for the older syntax out of the packaged directory,
// which is the safe direction — and both are re-read at the next start.
func (m *Manager) discover(ctx context.Context) {
	version, err := m.client.Version(ctx)
	if err != nil {
		m.log.Warn("reading the dhcp engine's version failed", "err", err)
	}
	cfg, err := m.client.ConfigGet(ctx)
	if err != nil {
		m.log.Warn("reading the dhcp engine's configuration failed", "err", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.version, m.hookDir, m.engineSocket = version, hookDirOf(cfg), socketNameOf(cfg)
}

// socketNameOf is the unix control socket the engine's own configuration
// names, under whichever key this engine spells it, or "" when it names
// none. It is rendered back verbatim (RenderInput.EngineSocket): the engine
// is listening there, whatever path this process dialled it by.
func socketNameOf(cfg map[string]any) string {
	dhcp4, _ := cfg["Dhcp4"].(map[string]any)
	if one, ok := dhcp4["control-socket"].(map[string]any); ok {
		if name, _ := one["socket-name"].(string); name != "" {
			return name
		}
	}
	many, _ := dhcp4["control-sockets"].([]any)
	for _, s := range many {
		entry, _ := s.(map[string]any)
		if entry["socket-type"] == "unix" {
			if name, _ := entry["socket-name"].(string); name != "" {
				return name
			}
		}
	}
	return ""
}

// hookDirOf is the directory the engine's own first hook library sits in,
// which is how Debian's and Alpine's layouts both work without a setting.
func hookDirOf(cfg map[string]any) string {
	dhcp4, _ := cfg["Dhcp4"].(map[string]any)
	hooks, _ := dhcp4["hooks-libraries"].([]any)
	for _, h := range hooks {
		entry, _ := h.(map[string]any)
		if lib, _ := entry["library"].(string); lib != "" {
			return path.Dir(lib)
		}
	}
	for _, dir := range hookDirs {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return hookDirs[0]
}

// Apply renders the current configuration and sends it as one config-set
// (§5.1). A config the engine refuses, or one that cannot be built at all,
// leaves the engine on what it was running and is recorded where the
// dashboard reads it; the next accepted render clears that.
func (m *Manager) Apply(ctx context.Context) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	in, err := m.inputs.RenderInput(ctx)
	if err != nil {
		return fmt.Errorf("reading the dhcp configuration: %w", err)
	}
	return m.apply(ctx, in)
}

// ApplyInput is Apply for a caller that has already read the configuration:
// it sends exactly that input rather than reading a second, possibly
// different one. The API path uses it to spend its budget on the engine
// rather than on the store it has just written.
func (m *Manager) ApplyInput(ctx context.Context, in RenderInput) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	return m.apply(ctx, in)
}

// ApplyIfChanged sends in only when the engine is not already running
// exactly what it renders to (§5.1). It is the gate, and it is here rather
// than in the caller because the comparison and the render have to be one
// critical section: two callers that compared before either rendered would
// have one of them claim the change and the other skip, and the one that
// skipped would return before the render it was responsible for had
// happened.
//
// "Already running" is what the engine was last *given* — accepted or
// refused. A configuration it refused is not re-sent while nothing about it
// has changed, which is §5.1's "nothing retries on a timer"; fixing the
// cause is a change, and so is pressing Apply again, which does not come
// through here.
func (m *Manager) ApplyIfChanged(ctx context.Context, in RenderInput) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	// Compared as the rendered Dhcp4 object rather than the input — the
	// input carries facts that never reach the engine, and LocalAddrs is all
	// of them: every IPv4 prefix on this host, re-read on every pass, so a
	// lease renewed on the WAN side or a VLAN brought up would have Kea
	// rebuild every subnet to arrive at what it was already running.
	// A render that cannot be built compares equal to nothing: the failure
	// is sent down the same path and refused, rather than skipped on a
	// comparison nothing established.
	cfg, err := m.build(in)
	if err == nil && hashOf(cfg) == m.lastHash() {
		return nil
	}
	return m.finish(ctx, in, cfg, err)
}

// apply is the render and the config-set. Callers hold applyMu.
func (m *Manager) apply(ctx context.Context, in RenderInput) error {
	cfg, err := m.build(in)
	return m.finish(ctx, in, cfg, err)
}

// finish is what happens to a build: the config-set when it built, the
// refusal when it did not, and the report either way.
//
// in is reported to Inputs.Rendered exactly as it arrived — the hook
// directory and the engine's version are added to the copy that is
// rendered, because they are what the engine said about itself rather than
// anything the app configured.
func (m *Manager) finish(ctx context.Context, in RenderInput, cfg map[string]any, err error) error {
	if err != nil {
		// A scope with no address to hand out as its resolver (§5.3) never
		// reaches the engine, and the operator fixes it the way they fix a
		// config the engine refused: by editing the scope.
		// The engine is not told, so it is not asked either: whether it is
		// there is still whatever the last exchange found, and there is no
		// configuration it was given to record.
		m.refuse(err.Error())
	} else {
		err = m.send(ctx, cfg)
	}
	m.inputs.Rendered(ctx, in, err)
	return err
}

func (m *Manager) send(ctx context.Context, cfg map[string]any) error {
	if err := m.client.ConfigSet(ctx, cfg); err != nil {
		var rejected *RejectedError
		switch {
		case errors.As(err, &rejected):
			// It answered, with a no — which is still a configuration it was
			// given, and re-sending it every poll is the retry §5.1 says
			// there is none of.
			m.refusedConfig(rejected.Text)
			m.setHash(hashOf(cfg))
		case Unanswered(err):
			// Not a refusal: the engine took no view of this config because
			// it was not there to take one, and the poll that finds it again
			// clears this and renders (§10). Nothing is recorded as given,
			// because it was not.
			m.unreachable(err)
		}
		return err
	}
	m.setHash(hashOf(cfg))
	m.accepted()
	return nil
}

// build is Render with the two facts the engine reported about itself filled
// in, and the input remembered for Status and the next poll. Both halves are
// wanted whether or not the render is sent: a poll that skips still saw the
// current scopes.
func (m *Manager) build(in RenderInput) (map[string]any, error) {
	m.mu.Lock()
	in.HookDir, in.KeaVersion, in.EngineSocket = m.hookDir, m.version, m.engineSocket
	m.mu.Unlock()
	m.remember(in)
	return Render(in)
}

// hashOf is one rendered configuration, identified. Render builds from
// map[string]any and []any precisely so that marshalling sorts the keys and
// two renders of the same input are byte-identical.
func hashOf(cfg map[string]any) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		// Unreachable for these types, and "" compares equal to nothing, so
		// the next comparison renders rather than skips.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) applyError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyErr
}

func (m *Manager) lastHash() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hash
}

func (m *Manager) setHash(sum string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hash = sum
}

// Unanswered reports whether err is a command the engine took no view of:
// one it never received (unreachable), one it received and let the deadline
// run out on, and one this process abandoned. The engine is left on
// whatever it was already running, and nothing has been learned about the
// configuration that was sent.
//
// The client keeps a deadline as the caller's own error, because it is the
// caller's clock — but which of the two noticed says nothing about the
// engine, and reading a deadline as anything else leaves "ok" on the
// dashboard for an engine that has stopped answering.
func Unanswered(err error) bool {
	return errors.Is(err, ErrUnreachable) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// Poll replaces the lease table with what the engine holds now (§7.1), and
// renders when what dnsaur would give the engine has moved. An engine that
// stops answering keeps the table it last gave: it is still the truth about
// the leases on the segment, and the dashboard says how old it is.
//
// The render is here because this is the only loop that runs without anyone
// doing anything, and three of §6's changes are exactly that: a replica
// pairs, is forgotten, or goes stale. None of them writes a setting — the
// registry is deliberately written without moving config_version, since a
// registration arrives from every replica every interval — so the settings
// watcher never wakes and no handler runs. The input this poll already read
// for the table is the same input a render is built from, so the render is
// one comparison away, and the comparison is what keeps it from sending
// anything on a poll where nothing moved.
func (m *Manager) Poll(ctx context.Context) error {
	leases, err := m.leases(ctx)
	if err != nil {
		// A cancelled poll is dnsaur shutting down, not the engine failing.
		if ctx.Err() == nil {
			m.pollFailed(err)
		}
		return err
	}
	in, read := m.config(ctx)
	scopes, classes, reservations, domain := m.remembered()
	table := tableFrom(leases, scopes, classes, reservations, domain)
	m.tableMu.Lock()
	m.table.Store(table)
	m.tableMu.Unlock()
	m.polled()
	m.readHA(ctx)
	m.notify(table)
	switch {
	case m.takeReturned():
		// An engine that has come back is serving whatever it started with,
		// not what dnsaur last rendered, and it may not even be the same
		// engine (§10). Ask it again what it is, and send it a config — this
		// one unconditionally, because what it is running is unknown.
		m.discover(ctx)
		if err := m.Apply(ctx); err != nil {
			m.log.Warn("rendering for the dhcp engine that came back failed", "err", err)
		}
	case read:
		// The convergence loop. Nothing is sent unless what the engine would
		// be given has actually changed. A configuration that cannot be
		// built is tried again every poll — nothing was given to the engine,
		// so nothing records it — and, like an engine that is down, it is on
		// the dashboard the whole time and in the log once.
		before := m.applyError()
		if err := m.ApplyIfChanged(ctx, in); err != nil {
			if m.applyError() != before {
				m.log.Warn("applying the changed dhcp configuration on a poll failed", "err", err)
			} else {
				m.log.Debug("applying the changed dhcp configuration on a poll failed", "err", err)
			}
		}
	}
	return nil
}

// leases pages through the whole lease database. Kea hands out one page per
// command and says nothing about how many there are, so the end is a page
// shorter than the one asked for.
func (m *Manager) leases(ctx context.Context) ([]Lease, error) {
	var all []Lease
	for from := ""; ; {
		page, next, err := m.client.LeasePage(ctx, from, leasePageLimit)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		from = next
	}
}

// config re-reads the configuration this poll works from, and reports
// whether it got one. A store that cannot be reached leaves the last input
// read in place — a table built against stale scopes is worth more than no
// lease table — and the false is what keeps that stale input from being
// rendered as if it were current.
func (m *Manager) config(ctx context.Context) (RenderInput, bool) {
	in, err := m.inputs.RenderInput(ctx)
	if err != nil {
		m.log.Warn("reading the dhcp configuration failed, polling against the last one read", "err", err)
		return RenderInput{}, false
	}
	m.remember(in)
	return in, true
}

// remembered is the last input's half the lease table is built from.
func (m *Manager) remembered() ([]store.Scope, []store.Class, []store.Reservation, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scopes, m.classes, m.reservations, m.domain
}

func (m *Manager) remember(in RenderInput) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scopes, m.classes, m.reservations, m.domain = in.Scopes, in.Classes, in.Reservations, in.Domain
}

// readHA reads the HA relationship on the same poll (§7.2). A single box has
// none and an engine too old to know the command refuses it; neither is a
// failed poll, and neither discards what was read last.
func (m *Manager) readHA(ctx context.Context) {
	_, ha, err := m.client.Status(ctx)
	if err != nil {
		m.log.Debug("reading the dhcp engine's status failed", "err", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ha = ha
}

func (m *Manager) pollFailed(err error) {
	var rejected *RejectedError
	changed := false
	switch {
	case Unanswered(err):
		changed = m.unreachable(err)
	case errors.As(err, &rejected):
		// Kea's own words, the way a refused config is shown (§8.4).
		changed = m.refusedPoll(rejected.Text)
	default:
		changed = m.refusedPoll(err.Error())
	}
	// An engine that has been down for a day must not write a line every
	// poll interval; the state is on the dashboard the whole time.
	if changed {
		m.log.Warn("reading dhcp leases failed", "err", err)
		return
	}
	m.log.Debug("reading dhcp leases failed", "err", err)
}

// Table is the leases as of the last successful poll.
func (m *Manager) Table() *Table { return m.table.Load() }

// Subscribe registers fn to be called with every new table. It runs on the
// polling goroutine, immediately after the swap, so a subscriber that blocks
// delays the next poll: keep it to swapping a pointer of your own.
func (m *Manager) Subscribe(fn func(*Table)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

func (m *Manager) notify(t *Table) {
	m.mu.Lock()
	subs := slices.Clone(m.subs)
	m.mu.Unlock()
	for _, fn := range subs {
		fn(t)
	}
}

// Release hands one address back to the pool. It is the engine's lease, not
// dnsaur's config, so nothing is rendered and the HA partner learns of it
// from Kea (§8.3).
//
// The table is corrected the moment the engine accepts, rather than at the
// next poll. Waiting would leave the row on the Leases page for up to
// dhcp.lease_poll_seconds after the operator released it — which reads as a
// button that did nothing — and would go on answering DNS with a name for an
// address the engine has already taken back.
func (m *Manager) Release(ctx context.Context, ip netip.Addr) error {
	if err := m.client.DeleteLease(ctx, ip.String()); err != nil {
		return err
	}
	m.tableMu.Lock()
	table := m.table.Load().without(ip)
	m.table.Store(table)
	m.tableMu.Unlock()
	m.notify(table)
	return nil
}

// Status answers §8.3's status object from what the last render and the last
// poll saw.
func (m *Manager) Status() Status {
	table := m.table.Load()
	leased := make(map[int64]int, len(table.entries))
	for _, e := range table.entries {
		// A reservation nothing has leased is a name, not an address in use.
		if !e.ExpiresAt.IsZero() {
			leased[e.ScopeID]++
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		Enabled:         true,
		Engine:          engineOK,
		EngineVersion:   m.version,
		TableAgeSeconds: int64(table.Age() / time.Second),
		HA:              m.ha,
		Scopes:          make([]ScopeUsage, 0, len(m.scopes)),
	}
	// A configuration the engine would not take outranks an engine that is
	// not there: it is the one an operator has to act on, and it is still
	// true when the engine comes back. A command refused on a poll is neither
	// — the engine is there and running a configuration dnsaur accepts — so
	// it is a message on an otherwise ok engine.
	switch {
	case m.applyErr != "":
		s.Engine, s.Message = engineRejected, m.applyErr
	case m.down:
		s.Engine, s.Message = engineDown, m.downErr
	default:
		s.Message = m.pollErr
	}
	for _, sc := range m.scopes {
		s.Scopes = append(s.Scopes, ScopeUsage{ID: sc.ID, PoolSize: poolSize(sc), Leased: leased[sc.ID]})
	}
	return s
}

// refusedConfig records a config-set the engine answered with a no: it is
// there, which is a reconnect if it had not been, and it is running something
// other than what dnsaur asked for.
//
// One critical section for both, because Status reads them together: between
// a bare "it answered" and the refusal being recorded, the dashboard would
// show an engine that is back and running dnsaur's configuration — which is
// the one thing that never happened here.
func (m *Manager) refusedConfig(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cameBack()
	m.down, m.downErr, m.applyErr = false, "", text
}

// cameBack notes an engine that has answered after being unreachable, for the
// next poll to act on. Callers hold mu.
//
// Only an engine something has actually failed to reach counts, which is what
// downErr says: the state a manager is constructed in is "nothing has
// answered yet" with no error to show for it, and Start discovers and renders
// unconditionally, so the first exchange of a run is owed nothing.
func (m *Manager) cameBack() {
	if m.down && m.downErr != "" {
		m.returned = true
	}
}

// takeReturned reports whether the engine has come back since the last time
// anything asked, and clears it: the reconnect is owed exactly one discovery
// and one render (§10).
func (m *Manager) takeReturned() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	returned := m.returned
	m.returned = false
	return returned
}

// unreachable records an engine that did not reply, and reports whether that
// is news.
func (m *Manager) unreachable(err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := !m.down || m.downErr != err.Error()
	m.down, m.downErr = true, err.Error()
	return changed
}

// refuse records a configuration that never took: one the engine would not
// run, or one that could not be built at all. It stands until a render is
// accepted (§5.1) — nothing else means the engine is running what dnsaur
// asked for, and a poll that reaches the engine says nothing about which
// configuration it is running.
func (m *Manager) refuse(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyErr = text
}

// accepted is a config-set the engine took: the one thing that clears a
// refusal, and an answer besides.
func (m *Manager) accepted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down, m.downErr, m.applyErr = false, "", ""
}

// refusedPoll records a command the engine would not run, and reports whether
// that is news. The engine is there — it answered, with a no — and the message
// goes when the next poll gets what it asked for.
func (m *Manager) refusedPoll(text string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := m.pollErr != text || m.down
	m.cameBack()
	m.down, m.downErr, m.pollErr = false, "", text
	return changed
}

// polled is a poll that got what it asked for: the engine is there, and
// whatever command it refused on the poll before has been run since.
func (m *Manager) polled() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cameBack()
	m.down, m.downErr, m.pollErr = false, "", ""
}

// loop polls until ctx ends, re-reading the interval every time: a setting
// changed on the dashboard takes effect on the next tick rather than at the
// next restart.
func (m *Manager) loop(ctx context.Context) {
	for {
		timer := time.NewTimer(m.interval(ctx))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		_ = m.Poll(ctx) // Poll records and logs its own failures.
	}
}

func (m *Manager) interval(ctx context.Context) time.Duration {
	n, err := m.settings.GetInt(ctx, pollSetting)
	if err != nil {
		n = defaultPollSeconds
	}
	return max(time.Duration(n), minPollSeconds) * time.Second
}

// poolSize is how many addresses a scope's pools hold, both ends of each
// included. A pool that does not parse is not a pool anything was handed out
// of, which is a size of zero rather than a guess. A reservations_only scope
// renders no pool at all, whatever it still lists, so its size is zero too.
func poolSize(s store.Scope) int {
	if s.ReservationsOnly {
		return 0
	}
	n := 0
	for _, p := range s.Pools {
		start, err := netip.ParseAddr(p.Start)
		if err != nil || !start.Is4() {
			continue
		}
		end, err := netip.ParseAddr(p.End)
		if err != nil || !end.Is4() {
			continue
		}
		from, to := start.As4(), end.As4()
		first, last := binary.BigEndian.Uint32(from[:]), binary.BigEndian.Uint32(to[:])
		if last >= first {
			n += int(last-first) + 1
		}
	}
	return n
}
