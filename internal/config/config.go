package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

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

	// Load YAML file if it exists
	if _, err := os.Stat(path); err == nil {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
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

	// Handle DNSAUR_DNS_LISTEN specially (comma-separated list)
	if v := os.Getenv("DNSAUR_DNS_LISTEN"); v != "" {
		if err := k.Set("dns_listen", strings.Split(v, ",")); err != nil {
			return nil, fmt.Errorf("set dns_listen: %w", err)
		}
	}

	// Build Config struct from koanf
	c := &Config{
		DNSListen:  k.Strings("dns_listen"),
		HTTPListen: k.String("http_listen"),
		DataDir:    k.String("data_dir"),
		LogLevel:   k.String("log_level"),
	}
	c.Storage.Driver = k.String("storage.driver")
	c.Storage.DSN = k.String("storage.dsn")

	// Validation
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
