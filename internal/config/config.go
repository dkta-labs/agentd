package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const DefaultListenAddress = "127.0.0.1:7337"

type Config struct {
	Listen         string      `json:"listen"`
	PublicURL      string      `json:"publicUrl"`
	DataDir        string      `json:"dataDir"`
	HerdrBinary    string      `json:"herdrBinary"`
	WorkspaceRoots []string    `json:"workspaceRoots"`
	Workspaces     []Workspace `json:"workspaces"`
	Auth           Auth        `json:"auth"`
	Push           Push        `json:"push"`
}

type Auth struct {
	Enabled      bool `json:"enabled"`
	CookieSecure bool `json:"cookieSecure"`
}

type Push struct {
	Enabled bool   `json:"enabled"`
	Subject string `json:"subject"`
}

type Workspace struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Path             string `json:"path"`
	HerdrSession     string `json:"-"`
	HerdrWorkspaceID string `json:"-"`
	HerdrSocketPath  string `json:"-"`
}

func Load(path string) (Config, error) {
	cfg, err := defaults()
	if err != nil {
		return Config{}, err
	}
	if path == "" {
		return cfg, nil
	}

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

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("resolve config directory: %w", err)
	}
	if err := cfg.normalize(baseDir); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaults() (Config, error) {
	dataDir, err := os.UserConfigDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	return Config{
		Listen:      DefaultListenAddress,
		DataDir:     filepath.Join(dataDir, "agentd"),
		HerdrBinary: "herdr",
	}, nil
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
	if strings.TrimSpace(cfg.Listen) == "" {
		return errors.New("listen address must not be empty")
	}
	cfg.PublicURL = strings.TrimSpace(cfg.PublicURL)
	if cfg.PublicURL != "" {
		publicURL, err := url.Parse(cfg.PublicURL)
		if err != nil || publicURL.Scheme != "https" || publicURL.Host == "" || publicURL.User != nil ||
			(publicURL.Path != "" && publicURL.Path != "/") || publicURL.RawQuery != "" || publicURL.Fragment != "" {
			return errors.New("publicUrl must be an HTTPS origin without credentials, path, query, or fragment")
		}
		publicURL.Path = ""
		cfg.PublicURL = publicURL.String()
	}
	if cfg.DataDir == "" {
		return errors.New("dataDir must not be empty")
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(baseDir, cfg.DataDir)
	}
	cfg.DataDir = filepath.Clean(cfg.DataDir)
	cfg.HerdrBinary = strings.TrimSpace(cfg.HerdrBinary)
	if cfg.HerdrBinary == "" {
		cfg.HerdrBinary = "herdr"
	}
	if cfg.Push.Enabled && !cfg.Auth.Enabled {
		return errors.New("push requires auth.enabled")
	}
	cfg.Push.Subject = strings.TrimSpace(cfg.Push.Subject)
	if cfg.Push.Enabled && !strings.HasPrefix(cfg.Push.Subject, "mailto:") && !strings.HasPrefix(cfg.Push.Subject, "https://") {
		return errors.New("push.subject must use mailto: or https://")
	}
	for i, root := range cfg.WorkspaceRoots {
		if strings.TrimSpace(root) == "" {
			return fmt.Errorf("workspaceRoots[%d] must not be empty", i)
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(baseDir, root)
		}
		cfg.WorkspaceRoots[i] = filepath.Clean(root)
	}

	seen := make(map[string]struct{}, len(cfg.Workspaces))
	for i := range cfg.Workspaces {
		workspace := &cfg.Workspaces[i]
		workspace.ID = strings.TrimSpace(workspace.ID)
		workspace.Name = strings.TrimSpace(workspace.Name)
		if workspace.ID == "" {
			return fmt.Errorf("workspaces[%d].id must not be empty", i)
		}
		if _, exists := seen[workspace.ID]; exists {
			return fmt.Errorf("duplicate workspace id %q", workspace.ID)
		}
		seen[workspace.ID] = struct{}{}
		if workspace.Name == "" {
			workspace.Name = workspace.ID
		}
		if workspace.Path == "" {
			return fmt.Errorf("workspace %q path must not be empty", workspace.ID)
		}
		if !filepath.IsAbs(workspace.Path) {
			workspace.Path = filepath.Join(baseDir, workspace.Path)
		}
		workspace.Path = filepath.Clean(workspace.Path)
	}
	return nil
}
