package config

import (
	"errors"
	"fmt"
	"log/slog"
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
	Storage    struct {
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

	// Handle DNSAUR_DNS_LISTEN specially (comma-separated list). The entries
	// are trimmed by listenAddrs below, along with the file's, so the two
	// ways of writing the same list produce the same value.
	if v := os.Getenv("DNSAUR_DNS_LISTEN"); v != "" {
		if err := k.Set("dns_listen", strings.Split(v, ",")); err != nil {
			return nil, fmt.Errorf("set dns_listen: %w", err)
		}
	}

	// Build Config struct from koanf
	c := &Config{
		DNSListen:  listenAddrs(k),
		HTTPListen: k.String("http_listen"),
		DataDir:    k.String("data_dir"),
		LogLevel:   k.String("log_level"),
	}
	c.Storage.Driver = k.String("storage.driver")
	c.Storage.DSN = k.String("storage.dsn")

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
