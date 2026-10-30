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

func TestMissingGitHubTokenFailsLoudlyNamingBothPlaces(t *testing.T) {
	for _, body := range []string{"", `{"anthropic_api_key":"` + anSecret + `"}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		if body != "" {
			p = writeCfg(t, body)
		}
		_, err := Load(p, env(nil))
		if err == nil {
			t.Fatal("started with no GitHub token")
		}
		for _, want := range []string{"github_token", "GITHUB_TOKEN"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
		if strings.Contains(err.Error(), anSecret) {
			t.Fatal("error printed a credential")
		}
	}
}

// Without an Anthropic key the service starts; the narrative runs in the browser.
func TestAnthropicKeyIsOptional(t *testing.T) {
	c, err := Load(writeCfg(t, `{"github_token":"`+ghSecret+`"}`), env(nil))
	if err != nil || c.AnthropicAPIKey != "" {
		t.Fatalf("%+v %v", c, err)
	}
	if c.BrowserModel != "onnx-community/VibeThinker-1.5B-ONNX" || len(c.BrowserModelRevision) != 40 || c.BrowserMaxNewTokens != 4096 {
		t.Fatalf("browser model defaults %+v", c)
	}
}

// A browser model must be pinned to a commit, so every viewer runs the same weights.
func TestBrowserModelMustBePinned(t *testing.T) {
	base := `"github_token":"a"`
	for _, extra := range []string{
		`"browser_model":"x/y"`, // a model without its revision
		`"browser_model":"x/y","browser_model_revision":"main"`,
		`"browser_model":"https://evil.example/x","browser_model_revision":"7295deafb5bc8cb447c007a9d246f64e9fb13cc8"`,
		`"browser_max_new_tokens":40000`,
	} {
		if _, err := Load(writeCfg(t, `{`+base+`,`+extra+`}`), env(nil)); err == nil {
			t.Errorf("accepted %s", extra)
		}
	}
	c, err := Load(writeCfg(t, `{`+base+`,"browser_model":"o/m","browser_model_revision":"0123456789abcdef0123456789abcdef01234567"}`), env(nil))
	if err != nil || c.BrowserModel != "o/m" {
		t.Fatalf("%+v %v", c, err)
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
	// Only the Anthropic key, from the environment: the GitHub token is missing and named.
	_, err = Load(filepath.Join(t.TempDir(), "absent.json"), env(map[string]string{EnvAnthropicKey: anSecret}))
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") || strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
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
