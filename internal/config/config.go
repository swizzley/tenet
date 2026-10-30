// Package config loads the service settings. The two credentials, the GitHub token
// and the Anthropic API key, come from config.json or from the environment
// (GITHUB_TOKEN, ANTHROPIC_API_KEY; the environment wins). Neither is ever written
// in code, and the service refuses to start, naming what is missing, if either is
// absent. Error messages name keys, never values.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Config is config.json. Unknown keys are an error, so a typo fails loudly.
type Config struct {
	GitHubToken     string `json:"github_token"`
	AnthropicAPIKey string `json:"anthropic_api_key"`

	Addr             string `json:"addr"`               // default 127.0.0.1:8080
	AnthropicModel   string `json:"anthropic_model"`    // default claude-opus-5-5
	BufferCap        int    `json:"buffer_cap"`         // records per buffer half, default 100
	DBPath           string `json:"db_path"`            // the SQLite file, default tenet.db
	MaxReconstruct   int    `json:"max_reconstruct"`    // commits one reconstruction may walk, default 300
	MaxPeriodDays    int    `json:"max_period_days"`    // longest insight period, default 366
	MaxPeriodCommits int    `json:"max_period_commits"` // commits one insight may read, default 20000
	CacheTTLSeconds  int    `json:"cache_ttl_seconds"`  // insight cache, default 600
	MaxSessions      int    `json:"max_sessions"`       // reconstructions running at once, default 4
}

// Env names. Each overrides its config.json key.
const (
	EnvGitHubToken  = "GITHUB_TOKEN"
	EnvAnthropicKey = "ANTHROPIC_API_KEY"
)

// Load reads path (a missing file is fine when the environment holds both
// credentials), applies the environment, fills defaults and validates.
func Load(path string, getenv func(string) string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return c, fmt.Errorf("config: read %s: %w", path, err)
	default:
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if v := strings.TrimSpace(getenv(EnvGitHubToken)); v != "" {
		c.GitHubToken = v
	}
	if v := strings.TrimSpace(getenv(EnvAnthropicKey)); v != "" {
		c.AnthropicAPIKey = v
	}
	c.GitHubToken = strings.TrimSpace(c.GitHubToken)
	c.AnthropicAPIKey = strings.TrimSpace(c.AnthropicAPIKey)

	var missing []string
	if c.GitHubToken == "" {
		missing = append(missing, fmt.Sprintf("github_token (in %s) or %s", path, EnvGitHubToken))
	}
	if c.AnthropicAPIKey == "" {
		missing = append(missing, fmt.Sprintf("anthropic_api_key (in %s) or %s", path, EnvAnthropicKey))
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("config: missing credentials: %s", strings.Join(missing, "; "))
	}

	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8080"
	}
	if c.AnthropicModel == "" {
		c.AnthropicModel = "claude-opus-5-5"
	}
	if c.DBPath == "" {
		c.DBPath = "tenet.db"
	}
	def(&c.BufferCap, 100)
	def(&c.MaxReconstruct, 300)
	def(&c.MaxPeriodDays, 366)
	def(&c.MaxPeriodCommits, 20000)
	def(&c.CacheTTLSeconds, 600)
	def(&c.MaxSessions, 4)
	for name, v := range map[string]int{"buffer_cap": c.BufferCap, "max_reconstruct": c.MaxReconstruct,
		"max_period_days": c.MaxPeriodDays, "max_period_commits": c.MaxPeriodCommits,
		"cache_ttl_seconds": c.CacheTTLSeconds, "max_sessions": c.MaxSessions} {
		if v < 1 {
			return c, fmt.Errorf("config: %s must be at least 1, got %d", name, v)
		}
	}
	if c.BufferCap > 100 {
		// One buffer half is one GraphQL page; GitHub serves at most 100 a page.
		return c, fmt.Errorf("config: buffer_cap must be at most 100 (one GitHub page), got %d", c.BufferCap)
	}
	return c, nil
}

// String prints the settings with the credentials masked.
func (c Config) String() string {
	m := c
	m.GitHubToken = mask(c.GitHubToken)
	m.AnthropicAPIKey = mask(c.AnthropicAPIKey)
	b, _ := json.Marshal(m)
	return string(b)
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}
