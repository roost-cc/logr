// Package config loads and saves logr configuration from $XDG_CONFIG_HOME/logr/.
//
// Server config: ~/.config/logr/server.toml
// Client config: ~/.config/logr/client.toml
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// ServerConfig controls logr-server behaviour.
type ServerConfig struct {
	DB           string `toml:"db"`
	IngestSocket string `toml:"ingest_socket"`
	QuerySocket  string `toml:"query_socket"`
	Retention    string `toml:"retention"`
}

// ClientConfig persists viewer state across sessions.
type ClientConfig struct {
	QuerySocket string          `toml:"query_socket"`
	Filter      string          `toml:"filter"`
	Columns     map[string]bool `toml:"columns"`
}

// IngestConfig configures a logr-in process: which socket to write to and
// which connector to run. CLI flags override individual fields.
type IngestConfig struct {
	IngestSocket string          `toml:"ingest_socket"`
	Connector    ConnectorConfig `toml:"connector"`
}

// ConnectorConfig describes a single connector and its arguments.
// Fields that don't apply to a connector type are ignored.
type ConnectorConfig struct {
	Type      string       `toml:"type"`      // "stdin" | "docker" | "cdp"
	Source    string       `toml:"source"`    // stdin: source label
	Parser    ParserConfig `toml:"parser"`    // all: parser name or per-service map
	Services  []string     `toml:"services"`  // docker: service names
	Endpoints []string     `toml:"endpoints"` // cdp: endpoint URLs
	Types     []string     `toml:"types"`     // cdp: target types filter
}

// ParserConfig holds either a global parser name (string) or a per-service
// map. In TOML either form is valid:
//
//	parser = "auto"           # global — all services use this parser
//
//	[connector.parser]        # per-service — unmapped services use "auto"
//	synapse   = "synapse"
//	assistant = "json"
type ParserConfig struct {
	global string
	perSvc map[string]string
}

// UnmarshalTOML handles both the string and table forms.
func (p *ParserConfig) UnmarshalTOML(v interface{}) error {
	switch val := v.(type) {
	case string:
		p.global = val
	case map[string]interface{}:
		p.perSvc = make(map[string]string, len(val))
		for k, v := range val {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("config: parser map value for %q must be a string", k)
			}
			p.perSvc[k] = s
		}
	default:
		return fmt.Errorf("config: parser must be a string or table, got %T", v)
	}
	return nil
}

// MarshalText encodes ParserConfig as a plain string for template generation.
// Per-service maps are only ever written by hand in the config file.
func (p ParserConfig) MarshalText() ([]byte, error) {
	if p.global != "" {
		return []byte(p.global), nil
	}
	return []byte("auto"), nil
}

// ForService returns the parser name to use for the given service.
// Returns "auto" when no specific parser is configured.
func (p ParserConfig) ForService(svc string) string {
	if p.perSvc != nil {
		if parser, ok := p.perSvc[svc]; ok {
			return parser
		}
		return "auto"
	}
	if p.global != "" {
		return p.global
	}
	return "auto"
}

// Resolve converts a -c value into an absolute config file path.
// A bare word (no path separator) expands to $XDG_CONFIG_HOME/logr/<name>.toml.
// Anything containing a "/" is used as-is.
func Resolve(nameOrPath string) string {
	if strings.ContainsRune(nameOrPath, '/') {
		return nameOrPath
	}
	return filepath.Join(xdgConfigHome(), "logr", nameOrPath+".toml")
}

func DefaultServer() ServerConfig {
	return ServerConfig{
		DB:           filepath.Join(xdgDataHome(), "logr", "logr.db"),
		IngestSocket: "unix:///tmp/logr-ingest.sock",
		QuerySocket:  "unix:///tmp/logr-query.sock",
		Retention:    "168h",
	}
}

func DefaultIngest() IngestConfig {
	return IngestConfig{
		IngestSocket: "unix:///tmp/logr-ingest.sock",
		Connector:    ConnectorConfig{Type: "stdin", Parser: ParserConfig{global: "line"}},
	}
}

// LoadIngest reads an ingest config from path.
// Pass an empty string to use the default location (~/.config/logr/ingest.toml);
// a missing default file is silently ignored. An explicit path that does not
// exist is an error.
func LoadIngest(path string) (IngestConfig, error) {
	cfg := DefaultIngest()
	explicit := path != ""
	if path == "" {
		path = filepath.Join(xdgConfigHome(), "logr", "ingest.toml")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if explicit {
			return cfg, fmt.Errorf("config file not found: %s", path)
		}
		return cfg, nil
	}
	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}

// WriteIngestTemplate writes a default ingest.toml at path (or the default
// location if path is empty). Does not overwrite an existing file.
func WriteIngestTemplate(path string) (string, error) {
	if path == "" {
		path = filepath.Join(xdgConfigHome(), "logr", "ingest.toml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, err
	}
	if _, err := os.Stat(path); err == nil {
		return path, fmt.Errorf("file already exists: %s", path)
	}
	f, err := os.Create(path)
	if err != nil {
		return path, err
	}
	defer f.Close()
	return path, toml.NewEncoder(f).Encode(DefaultIngest())
}

func DefaultClient() ClientConfig {
	return ClientConfig{
		QuerySocket: "unix:///tmp/logr-query.sock",
		Columns: map[string]bool{
			"log_time": true,
			"source":   true,
			"level":    true,
			"message":  true,
		},
	}
}

// LoadServer reads the server config from path.
// Pass an empty string to use the default location.
func LoadServer(path string) (ServerConfig, error) {
	cfg := DefaultServer()
	if path == "" {
		path = serverPath()
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}
	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}

// LoadClient reads the client config from path.
// Pass an empty string to use the default location.
func LoadClient(path string) (ClientConfig, error) {
	cfg := DefaultClient()
	if path == "" {
		path = clientPath()
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}
	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}

// SaveClient writes cfg to path. Pass an empty string to use the default location.
func SaveClient(cfg ClientConfig, path string) error {
	if path == "" {
		path = clientPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

// WriteServerTemplate writes a server.toml with defaults at path (or the default
// location if path is empty). Does not overwrite an existing file.
func WriteServerTemplate(path string) error {
	if path == "" {
		path = serverPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(DefaultServer())
}

func serverPath() string {
	return filepath.Join(xdgConfigHome(), "logr", "server.toml")
}

func clientPath() string {
	return filepath.Join(xdgConfigHome(), "logr", "client.toml")
}

func xdgConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

func xdgDataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}
