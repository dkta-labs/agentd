package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const DefaultListenAddress = "127.0.0.1:7337"

type Config struct {
	Listen            string            `json:"listen"`
	DataDir           string            `json:"dataDir"`
	HerdrBinary       string            `json:"herdrBinary"`
	AgentArgs         []string          `json:"agentArgs"`
	AgentEnv          map[string]string `json:"agentEnv"`
	CoordinatorTarget string            `json:"coordinatorTarget"`
	Workspaces        []Workspace       `json:"workspaces"`
}

type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func Load(path string) (Config, error) {
	cfg := Config{Listen: DefaultListenAddress, HerdrBinary: "herdr"}
	configDir := "."
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return Config{}, fmt.Errorf("open config: %w", err)
		}
		defer file.Close()
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("decode config: %w", err)
		}
		if err := rejectTrailingJSON(decoder); err != nil {
			return Config{}, err
		}
		configDir, err = filepath.Abs(filepath.Dir(path))
		if err != nil {
			return Config{}, fmt.Errorf("resolve config directory: %w", err)
		}
	}
	if cfg.DataDir == "" {
		root, err := os.UserConfigDir()
		if err != nil {
			return Config{}, fmt.Errorf("resolve user config directory: %w", err)
		}
		cfg.DataDir = filepath.Join(root, "agentd")
	}
	if err := cfg.normalize(configDir); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
func rejectTrailingJSON(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing config content: %w", err)
	}
	return errors.New("config must contain exactly one JSON object")
}
func (cfg *Config) normalize(baseDir string) error {
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	if cfg.Listen == "" {
		return errors.New("listen address must not be empty")
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen must be a host:port address: %w", err)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("listen address must be loopback-only")
		}
	}
	cfg.HerdrBinary = strings.TrimSpace(cfg.HerdrBinary)
	if cfg.HerdrBinary == "" {
		cfg.HerdrBinary = "herdr"
	}
	for i, arg := range cfg.AgentArgs {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			return fmt.Errorf("agentArgs[%d] must not be empty", i)
		}
		if reservedAgentArg(arg) {
			return fmt.Errorf("agentArgs[%d] conflicts with Agentd-owned interactive session arguments", i)
		}
		cfg.AgentArgs[i] = arg
	}
	normalizedEnv := make(map[string]string, len(cfg.AgentEnv))
	for rawName, rawValue := range cfg.AgentEnv {
		name := strings.TrimSpace(rawName)
		if !validEnvName(name) {
			return fmt.Errorf("agentEnv key %q is not a valid environment variable name", rawName)
		}
		if _, exists := normalizedEnv[name]; exists {
			return fmt.Errorf("agentEnv contains duplicate normalized key %q", name)
		}
		normalizedEnv[name] = rawValue
	}
	cfg.AgentEnv = normalizedEnv
	cfg.CoordinatorTarget = strings.TrimSpace(cfg.CoordinatorTarget)
	if cfg.CoordinatorTarget != "" && !validHerdrTarget(cfg.CoordinatorTarget) {
		return errors.New("coordinatorTarget must be a simple Herdr agent, pane, tab, or terminal target")
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return errors.New("dataDir must not be empty")
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(baseDir, cfg.DataDir)
	}
	cfg.DataDir = filepath.Clean(cfg.DataDir)
	seen := make(map[string]struct{}, len(cfg.Workspaces))
	for i := range cfg.Workspaces {
		w := &cfg.Workspaces[i]
		w.ID = strings.TrimSpace(w.ID)
		w.Name = strings.TrimSpace(w.Name)
		w.Path = strings.TrimSpace(w.Path)
		if w.ID == "" {
			return fmt.Errorf("workspaces[%d].id must not be empty", i)
		}
		if _, ok := seen[w.ID]; ok {
			return fmt.Errorf("duplicate workspace id %q", w.ID)
		}
		seen[w.ID] = struct{}{}
		if w.Name == "" {
			w.Name = w.ID
		}
		if w.Path == "" {
			return fmt.Errorf("workspace %q path must not be empty", w.ID)
		}
		if !filepath.IsAbs(w.Path) {
			w.Path = filepath.Join(baseDir, w.Path)
		}
		w.Path = filepath.Clean(w.Path)
	}
	return nil
}
func reservedAgentArg(arg string) bool {
	for _, flag := range []string{"-p", "--print", "--mode", "--cwd", "--no-session", "--session", "--session-dir", "--session-id", "--fork-session", "--continue", "-c", "--resume", "-r", "--"} {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
func validEnvName(name string) bool {
	if name == "" || !((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z') || name[0] == '_') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func validHerdrTarget(target string) bool {
	if len(target) == 0 || len(target) > 128 {
		return false
	}
	for i := range target {
		c := target[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}
func (cfg Config) Workspace(id string) (Workspace, error) {
	for _, w := range cfg.Workspaces {
		if w.ID == id {
			return w, nil
		}
	}
	return Workspace{}, fmt.Errorf("workspace %q is not configured", id)
}
