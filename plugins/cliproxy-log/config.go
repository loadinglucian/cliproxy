package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	defaultClickHouseURL   = "http://127.0.0.1:8123"
	defaultDatabase        = "cliproxy_log"
	defaultUser            = "cliproxy_writer"
	defaultSpoolDir        = "cliproxy-log-spool"
	defaultSpoolMaxBytes   = int64(5) << 30
	defaultMaxBodyBytes    = 32 << 20
	defaultFlushInterval   = 2 * time.Second
	defaultFlushRows       = 500
	defaultInflightTTL     = 30 * time.Minute
	defaultUsageGrace      = 5 * time.Second
	defaultMaxInflight     = 10000
	defaultQueueMaxBytes   = int64(512) << 20
	defaultBufferMaxBytes  = int64(256) << 20
	defaultFlushBatchBytes = 16 << 20
)

var databasePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pluginConfig is the parsed plugins.configs.cliproxy-log section. Durations and
// limits not exposed as keys use the defaults above.
type pluginConfig struct {
	ClickHouseURL          string
	ClickHouseDatabase     string
	ClickHouseUser         string
	ClickHousePasswordFile string
	SpoolDir               string
	SpoolMaxBytes          int64
	MaxBodyBytes           int
	// Machines maps the lowercase sha256 hex of a client API key to a machine name.
	Machines map[string]string

	FlushInterval  time.Duration
	FlushRows      int
	InflightTTL    time.Duration
	UsageGrace     time.Duration
	MaxInflight    int
	QueueMaxBytes  int64
	BufferMaxBytes int64
}

type rawConfig struct {
	ClickHouseURL          string            `yaml:"clickhouse-url"`
	ClickHouseDatabase     string            `yaml:"clickhouse-database"`
	ClickHouseUser         string            `yaml:"clickhouse-user"`
	ClickHousePasswordFile string            `yaml:"clickhouse-password-file"`
	SpoolDir               string            `yaml:"spool-dir"`
	SpoolMaxBytes          *int64            `yaml:"spool-max-bytes"`
	MaxBodyBytes           *int              `yaml:"max-body-bytes"`
	Machines               map[string]string `yaml:"machines"`
}

func defaultConfig() *pluginConfig {
	return &pluginConfig{
		ClickHouseURL:      defaultClickHouseURL,
		ClickHouseDatabase: defaultDatabase,
		ClickHouseUser:     defaultUser,
		SpoolDir:           defaultSpoolDir,
		SpoolMaxBytes:      defaultSpoolMaxBytes,
		MaxBodyBytes:       defaultMaxBodyBytes,
		Machines:           map[string]string{},
		FlushInterval:      defaultFlushInterval,
		FlushRows:          defaultFlushRows,
		InflightTTL:        defaultInflightTTL,
		UsageGrace:         defaultUsageGrace,
		MaxInflight:        defaultMaxInflight,
		QueueMaxBytes:      defaultQueueMaxBytes,
		BufferMaxBytes:     defaultBufferMaxBytes,
	}
}

// parseConfig validates the plugin YAML. Unknown keys (including the host-owned
// enabled and priority) are ignored. The password file is read at send time so it
// can be rotated without a reload.
func parseConfig(raw []byte) (*pluginConfig, error) {
	cfg := defaultConfig()
	var in rawConfig
	if len(raw) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, &in); errUnmarshal != nil {
			return nil, fmt.Errorf("parse cliproxy-log config: %w", errUnmarshal)
		}
	}
	if v := strings.TrimSpace(in.ClickHouseURL); v != "" {
		cfg.ClickHouseURL = strings.TrimRight(v, "/")
	}
	parsed, errURL := url.Parse(cfg.ClickHouseURL)
	if errURL != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("clickhouse-url must be an http(s) URL")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("clickhouse-url must not embed credentials; use clickhouse-user and clickhouse-password-file")
	}
	if v := strings.TrimSpace(in.ClickHouseDatabase); v != "" {
		cfg.ClickHouseDatabase = v
	}
	if !databasePattern.MatchString(cfg.ClickHouseDatabase) {
		return nil, fmt.Errorf("clickhouse-database must be a plain identifier")
	}
	if v := strings.TrimSpace(in.ClickHouseUser); v != "" {
		cfg.ClickHouseUser = v
	}
	cfg.ClickHousePasswordFile = strings.TrimSpace(in.ClickHousePasswordFile)
	if v := strings.TrimSpace(in.SpoolDir); v != "" {
		cfg.SpoolDir = v
	}
	if in.SpoolMaxBytes != nil {
		if *in.SpoolMaxBytes < 0 {
			return nil, fmt.Errorf("spool-max-bytes must not be negative")
		}
		cfg.SpoolMaxBytes = *in.SpoolMaxBytes
	}
	if in.MaxBodyBytes != nil {
		if *in.MaxBodyBytes <= 0 {
			return nil, fmt.Errorf("max-body-bytes must be positive")
		}
		cfg.MaxBodyBytes = *in.MaxBodyBytes
	}
	for hash, name := range in.Machines {
		hash = strings.ToLower(strings.TrimSpace(hash))
		if !sha256HexPattern.MatchString(hash) {
			return nil, fmt.Errorf("machines keys must be 64-character sha256 hex digests of client API keys")
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("machines entry has an empty name")
		}
		cfg.Machines[hash] = name
	}
	return cfg, nil
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "clickhouse-url", Type: pluginapi.ConfigFieldTypeString, Default: defaultClickHouseURL, Description: "ClickHouse HTTP endpoint."},
		{Name: "clickhouse-database", Type: pluginapi.ConfigFieldTypeString, Default: defaultDatabase, Description: "Database holding the requests table."},
		{Name: "clickhouse-user", Type: pluginapi.ConfigFieldTypeString, Default: defaultUser, Description: "ClickHouse user with INSERT on the requests table."},
		{Name: "clickhouse-password-file", Type: pluginapi.ConfigFieldTypeString, Description: "File containing the ClickHouse password; empty means no password."},
		{Name: "spool-dir", Type: pluginapi.ConfigFieldTypeString, Default: defaultSpoolDir, Description: "Directory for batches that could not be sent; relative to the proxy working directory."},
		{Name: "spool-max-bytes", Type: pluginapi.ConfigFieldTypeInteger, Default: defaultSpoolMaxBytes, Description: "Spool size cap; batches past it are dropped and counted."},
		{Name: "max-body-bytes", Type: pluginapi.ConfigFieldTypeInteger, Default: defaultMaxBodyBytes, Description: "Per-body capture cap; longer bodies are cut and the row is marked truncated."},
		{Name: "machines", Type: pluginapi.ConfigFieldTypeObject, Description: "Map of sha256 hex of a client API key to a machine name."},
	}
}
