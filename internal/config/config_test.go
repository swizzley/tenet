package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ghSecret = "ghp_secret_value_123"
	anSecret = "sk-ant-secret_value_456"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeCfg(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingBothFailsLoudlyNamingBoth(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "config.json"), env(nil))
	if err == nil {
		t.Fatal("started with no credentials")
	}
	for _, want := range []string{"github_token", "GITHUB_TOKEN", "anthropic_api_key", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestMissingOneNamesOnlyThatOne(t *testing.T) {
	p := writeCfg(t, `{"github_token":"`+ghSecret+`"}`)
	_, err := Load(p, env(nil))
	if err == nil || !strings.Contains(err.Error(), "anthropic_api_key") || strings.Contains(err.Error(), "github_token") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), ghSecret) {
		t.Fatal("error printed a credential")
	}
}

func TestFileOnly(t *testing.T) {
	c, err := Load(writeCfg(t, `{"github_token":"`+ghSecret+`","anthropic_api_key":"`+anSecret+`"}`), env(nil))
	if err != nil || c.GitHubToken != ghSecret || c.AnthropicAPIKey != anSecret {
		t.Fatalf("%+v %v", c, err)
	}
	if c.Addr != "127.0.0.1:8080" || c.DBPath != "tenet.db" || c.BufferCap != 100 || c.AnthropicModel != "claude-opus-5-5" {
		t.Fatalf("defaults %+v", c)
	}
}

func TestEnvOnlyNoFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.json"), env(map[string]string{EnvGitHubToken: ghSecret, EnvAnthropicKey: anSecret}))
	if err != nil || c.GitHubToken != ghSecret || c.AnthropicAPIKey != anSecret {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	p := writeCfg(t, `{"github_token":"from-file","anthropic_api_key":"from-file"}`)
	c, err := Load(p, env(map[string]string{EnvGitHubToken: "from-env"}))
	if err != nil || c.GitHubToken != "from-env" || c.AnthropicAPIKey != "from-file" {
		t.Fatalf("%+v %v", c, err)
	}
}

// Each credential is found on its own: one from the environment, the other from the file.
func TestEachCredentialFromEitherSource(t *testing.T) {
	p := writeCfg(t, `{"anthropic_api_key":"`+anSecret+`"}`)
	c, err := Load(p, env(map[string]string{EnvGitHubToken: ghSecret}))
	if err != nil || c.GitHubToken != ghSecret || c.AnthropicAPIKey != anSecret {
		t.Fatalf("%+v %v", c, err)
	}
	p = writeCfg(t, `{"github_token":"`+ghSecret+`"}`)
	c, err = Load(p, env(map[string]string{EnvAnthropicKey: anSecret}))
	if err != nil || c.GitHubToken != ghSecret || c.AnthropicAPIKey != anSecret {
		t.Fatalf("%+v %v", c, err)
	}
	// Only the GitHub token, from the environment: the Anthropic key is missing and named.
	_, err = Load(filepath.Join(t.TempDir(), "absent.json"), env(map[string]string{EnvGitHubToken: ghSecret}))
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestBlankCountsAsMissing(t *testing.T) {
	p := writeCfg(t, `{"github_token":"   ","anthropic_api_key":"`+anSecret+`"}`)
	if _, err := Load(p, env(nil)); err == nil || !strings.Contains(err.Error(), "github_token") {
		t.Fatalf("err = %v", err)
	}
}

func TestTypoAndBadJSONFail(t *testing.T) {
	if _, err := Load(writeCfg(t, `{"github_tokn":"x","anthropic_api_key":"y"}`), env(nil)); err == nil {
		t.Fatal("accepted an unknown key")
	}
	if _, err := Load(writeCfg(t, `{`), env(nil)); err == nil {
		t.Fatal("accepted broken JSON")
	}
}

func TestBadLimitsFail(t *testing.T) {
	base := `"github_token":"a","anthropic_api_key":"b"`
	for _, extra := range []string{`"buffer_cap":101`, `"buffer_cap":-1`, `"max_sessions":-3`} {
		if _, err := Load(writeCfg(t, `{`+base+`,`+extra+`}`), env(nil)); err == nil {
			t.Errorf("accepted %s", extra)
		}
	}
}

func TestStringMasksCredentials(t *testing.T) {
	c, _ := Load(writeCfg(t, `{"github_token":"`+ghSecret+`","anthropic_api_key":"`+anSecret+`"}`), env(nil))
	s := c.String()
	if strings.Contains(s, ghSecret) || strings.Contains(s, anSecret) || !strings.Contains(s, "***") {
		t.Fatalf("String() = %s", s)
	}
}
