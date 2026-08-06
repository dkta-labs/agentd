package githubhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const DefaultListen = "127.0.0.1:7338"

type Config struct {
	Listen     string `json:"listen"`
	AgentdURL  string `json:"agentdUrl"`
	SecretFile string `json:"secretFile"`
	DataDir    string `json:"dataDir"`
	Rules      []Rule `json:"rules"`
}

type Rule struct {
	ID         string `json:"id"`
	Event      string `json:"event"`
	Action     string `json:"action"`
	Repository string `json:"repository"`
	Merged     *bool  `json:"merged,omitempty"`
	JobID      string `json:"jobId"`
}

func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	var cfg Config
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("config must contain exactly one JSON object")
		}
		return Config{}, fmt.Errorf("decode trailing config: %w", err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("resolve config directory: %w", err)
	}
	if err := cfg.normalize(base); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg *Config) normalize(base string) error {
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if err := validateLoopbackAddress(cfg.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	cfg.AgentdURL = strings.TrimRight(strings.TrimSpace(cfg.AgentdURL), "/")
	if cfg.AgentdURL == "" {
		cfg.AgentdURL = "http://127.0.0.1:7337"
	}
	parsed, err := url.Parse(cfg.AgentdURL)
	if err != nil {
		return fmt.Errorf("agentdUrl: %w", err)
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("agentdUrl must be an origin-only loopback HTTP URL")
	}
	if err := validateLoopbackAddress(parsed.Host); err != nil {
		return fmt.Errorf("agentdUrl: %w", err)
	}
	cfg.SecretFile = strings.TrimSpace(cfg.SecretFile)
	if cfg.SecretFile == "" {
		return errors.New("secretFile is required")
	}
	if !filepath.IsAbs(cfg.SecretFile) {
		cfg.SecretFile = filepath.Join(base, cfg.SecretFile)
	}
	cfg.SecretFile = filepath.Clean(cfg.SecretFile)
	cfg.DataDir = strings.TrimSpace(cfg.DataDir)
	if cfg.DataDir == "" {
		return errors.New("dataDir is required")
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(base, cfg.DataDir)
	}
	cfg.DataDir = filepath.Clean(cfg.DataDir)
	if len(cfg.Rules) == 0 {
		return errors.New("at least one rule is required")
	}
	ids := make(map[string]struct{}, len(cfg.Rules))
	for index := range cfg.Rules {
		rule := &cfg.Rules[index]
		rule.ID = strings.TrimSpace(rule.ID)
		rule.Event = strings.TrimSpace(rule.Event)
		rule.Action = strings.TrimSpace(rule.Action)
		rule.Repository = strings.TrimSpace(rule.Repository)
		rule.JobID = strings.TrimSpace(rule.JobID)
		if !validName(rule.ID) {
			return fmt.Errorf("rules[%d].id must be a simple name", index)
		}
		if _, exists := ids[rule.ID]; exists {
			return fmt.Errorf("duplicate rule id %q", rule.ID)
		}
		ids[rule.ID] = struct{}{}
		if !validName(rule.Event) || !validName(rule.Action) {
			return fmt.Errorf("rules[%d] event and action must be simple names", index)
		}
		parts := strings.Split(rule.Repository, "/")
		if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
			return fmt.Errorf("rules[%d].repository must be owner/name", index)
		}
		if !validName(rule.JobID) {
			return fmt.Errorf("rules[%d].jobId must be a simple name", index)
		}
		if rule.Merged != nil && rule.Event != "pull_request" {
			return fmt.Errorf("rules[%d].merged is supported only for pull_request events", index)
		}
	}
	return nil
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("must be a host:port address")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("must be loopback-only")
	}
	return nil
}

func validName(value string) bool {
	if value == "" || len(value) > 200 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func LoadSecret(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect secret file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("secret file must be regular and mode 0600 or stricter")
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	if len(secret) > 0 && secret[len(secret)-1] == '\n' {
		secret = secret[:len(secret)-1]
		if len(secret) > 0 && secret[len(secret)-1] == '\r' {
			secret = secret[:len(secret)-1]
		}
	}
	if len(secret) < 32 || len(secret) > 4096 {
		return nil, errors.New("secret must contain between 32 and 4096 bytes")
	}
	return secret, nil
}
