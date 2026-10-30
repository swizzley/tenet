// Package config loads the service settings. The credentials, the GitHub token
// and the optional Anthropic API key, come from config.json or from the environment
// (GITHUB_TOKEN, ANTHROPIC_API_KEY; the environment wins). Neither is ever written
// in code. The service refuses to start, naming where to put it, if the GitHub
// token is absent. Without an Anthropic key the narrative runs in the browser only.
// Error messages name keys, never values.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// Config is config.json. Unknown keys are an error, so a typo fails loudly.
type Config struct {
	GitHubToken     string `json:"github_token"`
	AnthropicAPIKey string `json:"anthropic_api_key"` // optional: enables the server-side narrative

	Addr             string `json:"addr"`               // default 127.0.0.1:8080
	AnthropicModel   string `json:"anthropic_model"`    // default claude-opus-5-5
	BufferCap        int    `json:"buffer_cap"`         // records per buffer half, default 100
	DBPath           string `json:"db_path"`            // the SQLite file, default tenet.db
	MaxReconstruct   int    `json:"max_reconstruct"`    // commits one reconstruction may walk, default 1000
	MaxPeriodDays    int    `json:"max_period_days"`    // longest insight period, default 366
	MaxPeriodCommits int    `json:"max_period_commits"` // commits one insight may read, default 20000
	CacheTTLSeconds  int    `json:"cache_ttl_seconds"`  // insight cache, default 600
	MaxSessions      int    `json:"max_sessions"`       // reconstructions running at once, default 4

	// The model the page runs in the browser (WebGPU) for the narrative.
	BrowserModel         string `json:"browser_model"`          // a Hugging Face ONNX repo, default onnx-community/VibeThinker-1.5B-ONNX
	BrowserModelRevision string `json:"browser_model_revision"` // a full commit id, so every viewer loads the same weights
	BrowserMaxNewTokens  int    `json:"browser_max_new_tokens"` // the reasoning budget, default 4096 (the answer has its own)
}

// Env names. Each overrides its config.json key.
const (
	EnvGitHubToken  = "GITHUB_TOKEN"
	EnvAnthropicKey = "ANTHROPIC_API_KEY"
)

var (
	hfRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Load reads path (a missing file is fine when the environment holds the
// GitHub token), applies the environment, fills defaults and validates.
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
	if c.GitHubToken == "" {
		return c, fmt.Errorf("config: missing credential: github_token (in %s) or %s", path, EnvGitHubToken)
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
	if c.BrowserModel == "" && c.BrowserModelRevision == "" {
		// VERIFIED 2026-10-30: the q4f16 build is 1.24 GB; the commit is the repo's head that day.
		c.BrowserModel = "onnx-community/VibeThinker-1.5B-ONNX"
		c.BrowserModelRevision = "7295deafb5bc8cb447c007a9d246f64e9fb13cc8"
	}
	def(&c.BufferCap, 100)
	def(&c.MaxReconstruct, 1000)
	def(&c.MaxPeriodDays, 366)
	def(&c.MaxPeriodCommits, 20000)
	def(&c.CacheTTLSeconds, 600)
	def(&c.MaxSessions, 4)
	def(&c.BrowserMaxNewTokens, 4096)
	for name, v := range map[string]int{"buffer_cap": c.BufferCap, "max_reconstruct": c.MaxReconstruct,
		"max_period_days": c.MaxPeriodDays, "max_period_commits": c.MaxPeriodCommits,
		"cache_ttl_seconds": c.CacheTTLSeconds, "max_sessions": c.MaxSessions,
		"browser_max_new_tokens": c.BrowserMaxNewTokens} {
		if v < 1 {
			return c, fmt.Errorf("config: %s must be at least 1, got %d", name, v)
		}
	}
	if c.BufferCap > 100 {
		// One buffer half is one GraphQL page; GitHub serves at most 100 a page.
		return c, fmt.Errorf("config: buffer_cap must be at most 100 (one GitHub page), got %d", c.BufferCap)
	}
	if c.BrowserMaxNewTokens > 32768 {
		return c, fmt.Errorf("config: browser_max_new_tokens must be at most 32768, got %d", c.BrowserMaxNewTokens)
	}
	if !hfRepoRe.MatchString(c.BrowserModel) {
		return c, fmt.Errorf("config: browser_model must be a Hugging Face repo id like owner/name, got %q", c.BrowserModel)
	}
	if !commitRe.MatchString(c.BrowserModelRevision) {
		return c, errors.New("config: browser_model_revision must be a full 40-character commit id, so every viewer loads the same weights")
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
