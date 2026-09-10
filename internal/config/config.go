package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// DefaultPath is the bootstrap file Load reads when -config names nothing
// else. It is the one path allowed to be absent: running with no config file
// at all is a supported setup, while a path the operator typed and that is
// not there is a typo.
const DefaultPath = "dnsaur.yaml"

type Config struct {
	DNSListen  []string `yaml:"dns_listen"`
	HTTPListen string   `yaml:"http_listen"`
	DataDir    string   `yaml:"data_dir"`
	LogLevel   string   `yaml:"log_level"`
	// TrustedProxies are the networks a reverse proxy in front of dnsaur
	// may connect from. A request arriving from one of them has its
	// X-Forwarded-Proto and X-Forwarded-For headers believed; a request
	// from anywhere else does not, because those headers are just headers
	// and any client can send them. Empty (the default) trusts nothing,
	// which is the right answer for a dnsaur exposed directly.
	//
	// Tagged "-" because the struct decoder cannot read this key: a bare
	// address is accepted here and netip.ParsePrefix refuses one. Load
	// fills the field from trusted_proxies through its own reader.
	TrustedProxies []netip.Prefix `yaml:"-"`
	Storage        struct {
		Driver string `yaml:"driver"`
		DSN    string `yaml:"dsn"`
	} `yaml:"storage"`
}

// listenAddrs reads dns_listen from every shape it can arrive in and
// normalises the lot.
//
// A scalar (`dns_listen: ":53"`) is the natural thing to write for a single
// address, and koanf's Strings answers a scalar with nothing at all — which
// bound no DNS listener and left the server running, answering no queries,
// with an empty address in the startup log as the only trace. It is read
// here as the one-element list it plainly means.
//
// Entries are trimmed and blanks dropped because every one of them is handed
// to net.Listen verbatim: " :5353" from a spaced-out env list, or "" from a
// trailing comma, is not an address. An empty result is a real error, raised
// by the caller rather than here, so "no addresses" and "the addresses given
// were all blank" fail the same way.
func listenAddrs(k *koanf.Koanf) []string {
	raw := k.Strings("dns_listen")
	if s, ok := k.Get("dns_listen").(string); ok {
		raw = []string{s}
	}
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// trustedProxies reads trusted_proxies from either shape it can arrive in
// — a YAML list or a comma-separated env value, both already flattened by
// koanf — and parses each entry.
//
// A bare address is accepted as the single host it plainly means
// ("192.168.1.5" for a proxy on one machine, which is the common case)
// rather than being refused for missing a prefix length. An entry that is
// neither an address nor a CIDR is an error: silently dropping it would
// leave the operator with a proxy they believe is trusted and a session
// cookie shipping without Secure, with nothing anywhere saying why.
func trustedProxies(k *koanf.Koanf) ([]netip.Prefix, error) {
	raw := k.Strings("trusted_proxies")
	if s, ok := k.Get("trusted_proxies").(string); ok {
		raw = strings.Split(s, ",")
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, e := range raw {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if pfx, err := netip.ParsePrefix(e); err == nil {
			out = append(out, pfx.Masked())
			continue
		}
		addr, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies %q is not an IP address or CIDR", e)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

func Load(path string) (*Config, error) {
	k := koanf.New(".")

	// Load defaults via confmap provider
	defaults := map[string]interface{}{
		"dns_listen":  []string{":53"},
		"http_listen": ":8080",
		"data_dir":    "./data",
		"log_level":   "info",
		"storage": map[string]interface{}{
			"driver": "sqlite",
		},
	}
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}

	// Load the YAML file. Absence is only tolerable for DefaultPath: a path
	// the operator named and that is not there is a typo, and starting on
	// defaults leaves it running with none of the configuration they wrote
	// and nothing saying so.
	switch _, err := os.Stat(path); {
	case err == nil:
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && path == DefaultPath:
	default:
		return nil, err
	}

	// Apply environment variable overrides (env > file > defaults)
	// Using explicit map for precise control over env key → koanf path mapping
	envMap := map[string]string{
		"DNSAUR_HTTP_LISTEN":    "http_listen",
		"DNSAUR_DATA_DIR":       "data_dir",
		"DNSAUR_LOG_LEVEL":      "log_level",
		"DNSAUR_STORAGE_DRIVER": "storage.driver",
		"DNSAUR_STORAGE_DSN":    "storage.dsn",
	}
	for envKey, koanfKey := range envMap {
		if v := os.Getenv(envKey); v != "" {
			if err := k.Set(koanfKey, v); err != nil {
				return nil, fmt.Errorf("set %s: %w", koanfKey, err)
			}
		}
	}

	if v := os.Getenv("DNSAUR_TRUSTED_PROXIES"); v != "" {
		if err := k.Set("trusted_proxies", strings.Split(v, ",")); err != nil {
			return nil, fmt.Errorf("set trusted_proxies: %w", err)
		}
	}

	// Handle DNSAUR_DNS_LISTEN specially (comma-separated list). The entries
	// are trimmed by listenAddrs below, along with the file's, so the two
	// ways of writing the same list produce the same value.
	if v := os.Getenv("DNSAUR_DNS_LISTEN"); v != "" {
		if err := k.Set("dns_listen", strings.Split(v, ",")); err != nil {
			return nil, fmt.Errorf("set dns_listen: %w", err)
		}
	}

	// koanf fills the plain fields straight from their yaml tags. The two
	// that are not plain keep their own readers and run after the decode,
	// so what those return is what the caller gets: dns_listen accepts a
	// scalar as the one-element list it plainly means, and trusted_proxies
	// accepts a bare address as the single host it plainly means.
	c := &Config{}
	if err := k.UnmarshalWithConf("", c, koanf.UnmarshalConf{Tag: "yaml"}); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	c.DNSListen = listenAddrs(k)
	proxies, err := trustedProxies(k)
	if err != nil {
		return nil, err
	}
	c.TrustedProxies = proxies

	// Validation
	if len(c.DNSListen) == 0 {
		return nil, fmt.Errorf("dns_listen must name at least one address")
	}
	// Parsed with the same call cmd/dnsaur uses to configure the handler, so
	// a value that loads here is a value that will set the level there. It
	// used to be parsed only there, with the error discarded, which made
	// every misspelling silently INFO.
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return nil, fmt.Errorf("unknown log_level %q: must be one of debug, info, warn, error", c.LogLevel)
	}
	if c.Storage.Driver != "sqlite" && c.Storage.Driver != "postgres" {
		return nil, fmt.Errorf("unknown storage driver %q", c.Storage.Driver)
	}
	if c.Storage.DSN == "" {
		if c.Storage.Driver == "postgres" {
			return nil, fmt.Errorf("storage.dsn required for postgres")
		}
		c.Storage.DSN = filepath.Join(c.DataDir, "dnsaur.db")
	}

	return c, nil
}
