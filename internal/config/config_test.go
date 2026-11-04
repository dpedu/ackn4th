package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadLayersOverDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)
	os.MkdirAll(filepath.Join(dir, "config"), 0o700)
	// session_ttl is gone: a file that still has it loads.
	os.WriteFile(filepath.Join(dir, "config", "config.yaml"), []byte(
		"session_ttl: 8h\neffects: false\nproviders:\n  claude:\n    model: claude-sonnet-5-5\n"), 0o600)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Effects {
		t.Errorf("file values not applied: %+v", cfg)
	}
	c := cfg.Providers["claude"]
	if c.Model != "claude-sonnet-5-5" || c.MaxTokens != 64000 || c.Effort != "high" {
		t.Errorf("partial provider not backfilled: %+v", c)
	}
	if cfg.Providers["gemini"].Model == "" || cfg.Limits.MaxRounds != 10 {
		t.Errorf("defaults lost: %+v", cfg)
	}
}

func TestMissingFileGivesDefaults(t *testing.T) {
	t.Setenv(HomeEnv, t.TempDir())
	cfg, err := Load()
	if err != nil || !cfg.Effects {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestResolveAgent(t *testing.T) {
	c := Default()
	c.Agents = map[string]AgentProfile{"fast": {Provider: "claude", Model: "claude-sonnet-5-5", Effort: "low"}}

	p, err := c.ResolveAgent("", AgentProfile{Provider: "gemini"})
	if err != nil || p.Model != c.Providers["gemini"].Model || p.CompactAt != 200000 || p.MaxTokens != 65536 {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	p, err = c.ResolveAgent("fast", AgentProfile{Effort: "high"})
	if err != nil || p.Provider != "claude" || p.Model != "claude-sonnet-5-5" || p.Effort != "high" || p.MaxTokens != 64000 {
		t.Fatalf("profile + override: %+v %v", p, err)
	}
	for name, tc := range map[string]struct {
		profile string
		set     AgentProfile
	}{
		"missing profile":   {"nope", AgentProfile{}},
		"provider conflict": {"fast", AgentProfile{Provider: "gemini"}},
		"no provider":       {"", AgentProfile{}},
		"bad effort":        {"", AgentProfile{Provider: "claude", Effort: "turbo"}},
		"tiny compact_at":   {"", AgentProfile{Provider: "claude", CompactAt: 1000}},
	} {
		if _, err := c.ResolveAgent(tc.profile, tc.set); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestGeminiAuth(t *testing.T) {
	for _, c := range []struct {
		p    Provider
		want string
	}{
		{Provider{}, GeminiKey},
		{Provider{Project: "p"}, GeminiVertex},
		{Provider{Project: "p", Auth: GeminiKey}, GeminiKey},
		{Provider{Auth: GeminiVertex}, GeminiVertex},
	} {
		if got := c.p.GeminiAuth(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.p, got, c.want)
		}
	}
}

func TestSetKeyKeepsTheRest(t *testing.T) {
	t.Setenv(HomeEnv, t.TempDir())
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetKey("tips", false); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.Tips {
		t.Fatalf("tips should be off: %v %v", c.Tips, err)
	}
	if err := os.WriteFile(path, []byte("# mine\ndefault_provider: gemini\ntips: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetKey("tips", true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if s := string(data); !strings.Contains(s, "# mine") || !strings.Contains(s, "default_provider: gemini") || !strings.Contains(s, "tips: true") {
		t.Errorf("SetKey rewrote the file:\n%s", s)
	}
}

func TestSetKeyNested(t *testing.T) {
	t.Setenv(HomeEnv, t.TempDir())
	path, _ := Path()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("# mine\nproviders:\n  openai: {model: old, effort: low}\n"), 0o600)
	if _, err := SetKey("providers.openai.model", "gpt-new"); err != nil {
		t.Fatal(err)
	}
	if _, err := SetKey("providers.gemini.model", "gem-new"); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if o := c.Providers["openai"]; o.Model != "gpt-new" || o.Effort != "low" || c.Providers["gemini"].Model != "gem-new" {
		t.Errorf("providers = %+v", c.Providers)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "# mine") {
		t.Errorf("comment lost:\n%s", data)
	}
	if _, err := SetKey("providers.openai.model.x", 1); err == nil {
		t.Error("set a key inside a scalar")
	}
}

// Config and state go where each OS keeps an app's files.
func TestOSLocations(t *testing.T) {
	t.Setenv(HomeEnv, "")
	cfg, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := map[string][2]string{
		"darwin":  {filepath.Join(home, "Library", "Application Support", "ackn4th"), filepath.Join(home, "Library", "Application Support", "ackn4th")},
		"windows": {filepath.Join(os.Getenv("APPDATA"), "ackn4th"), filepath.Join(os.Getenv("LOCALAPPDATA"), "ackn4th")},
	}[runtime.GOOS]
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("XDG_DATA_HOME", "")
		cfg, _ = Dir()
		data, _ = DataDir()
		want = [2]string{filepath.Join(home, ".config", "ackn4th"), filepath.Join(home, ".local", "share", "ackn4th")}
	}
	if want[0] != "" && (cfg != want[0] || data != want[1]) {
		t.Errorf("config %s, data %s; want %s, %s", cfg, data, want[0], want[1])
	}
}

func TestBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://api.deepseek.com":       "https://api.deepseek.com/",
		"https://example.com/v1/":        "https://example.com/v1/",
		"http://localhost:11434/v1":      "http://localhost:11434/v1/",
		"http://127.0.0.1:8000":          "http://127.0.0.1:8000/",
		"http://api.deepseek.com":        "",
		"https://user:pw@example.com":    "",
		"https://example.com/?key=x":     "",
		"ftp://example.com":              "",
		"api.deepseek.com":               "",
		"http://localhost.evil.com:8000": "",
	} {
		got, err := CheckBaseURL(raw)
		if want == "" && err == nil {
			t.Errorf("%s: accepted as %s", raw, got)
		} else if want != "" && got != want {
			t.Errorf("%s: %q, %v; want %s", raw, got, err, want)
		}
	}

	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)
	os.MkdirAll(filepath.Join(dir, "config"), 0o700)
	write := func(s string) { os.WriteFile(filepath.Join(dir, "config", "config.yaml"), []byte(s), 0o600) }
	write("providers:\n  openai: {base_url: https://api.deepseek.com, model: deepseek-v4-pro}\n")
	cfg, err := Load()
	if err != nil || cfg.Providers["openai"].BaseURL != "https://api.deepseek.com/" {
		t.Errorf("openai base_url: %+v, %v", cfg.Providers["openai"], err)
	}
	write("providers:\n  claude: {base_url: https://example.com}\n")
	if _, err := Load(); err == nil {
		t.Error("base_url on claude accepted")
	}
}
