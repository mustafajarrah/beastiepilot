package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSettings(t *testing.T) {
	input := `
# a comment
MODEL=qwen2.5-coder
EMBED_MODEL=mxbai-embed-large
endpoint = "http://10.0.0.5:11434/api/generate"   # quoted, with a comment
TIMEOUT='90s'
CONTEXT=8192
history = 25   # how many go along
type=GUIDED
AUTO_RUN = df ,  zfs   list, service * status
PACKAGE_MANAGER=PORTS # trailing comment
SOURCE_PATH=/usr/obj/../src-14/
DOCS_SOURCE=
RAG_SOURCE="/home/me/My Books"
BANNER=NO
Verbose=true
`
	s := defaultSettings()
	if err := s.parse(strings.NewReader(input), "test.conf"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := settings{
		Model:          "qwen2.5-coder",
		EmbedModel:     "mxbai-embed-large",
		Endpoint:       "http://10.0.0.5:11434/api/generate",
		Timeout:        90 * time.Second,
		Context:        8192,
		History:        25,
		Type:           typeGuided,
		AutoRun:        "df, zfs list, service * status",
		PackageManager: "ports",
		SourcePath:     "/usr/src-14",
		DocsSource:     "",
		RagSource:      "/home/me/My Books",
		Banner:         bannerOff,
		Verbose:        true,
	}
	if s != want {
		t.Errorf("parse gave\n %+v\nwant\n %+v", s, want)
	}
}

func TestParseSettingsErrors(t *testing.T) {
	tests := []struct{ input, want string }{
		{"MODEL=x\nTYPO=1\n", "test.conf:2: TYPO: unknown setting"},
		{"just words\n", "test.conf:1: expected KEY=value"},
		{"\n\nTIMEOUT=soon\n", "test.conf:3: TIMEOUT:"},
		{"TIMEOUT=-5s\n", "not a positive duration"},
		{"CONTEXT=2048\n", "CONTEXT: \"2048\" is not a number of tokens from 4096 to 131072"},
		{"CONTEXT=8k\n", "is not a number of tokens"},
		{"CONTEXT=1000000\n", "is not a number of tokens"},
		{"HISTORY=-1\n", "HISTORY: \"-1\" is not a number of requests from 0 to 1000"},
		{"HISTORY=all\n", "is not a number of requests"},
		{"HISTORY=1001\n", "is not a number of requests"},
		{"HISTORY=\n", "is not a number of requests"},
		{"PACKAGE_MANAGER=apt\n", "use pkg or ports"},
		{"VERBOSE=maybe\n", "not a boolean"},
		{"TYPE=auto\n", "use guided or safe"},
		{"TYPE=unsafe\n", "use guided or safe"},
		{"AUTO_RUN=df, sh\n", "test.conf:1: AUTO_RUN: \"sh\" runs other commands"},
		{"BANNER=huge\n", "use full, compact or off"},
		{"MODEL=\n", "MODEL: must not be empty"},
		{"EMBED_MODEL=\n", "EMBED_MODEL: must not be empty"},
		{"RAG_SOURCE=books\n", "test.conf:1: RAG_SOURCE: \"books\" is not an absolute path"},
		{"SOURCE_PATH=./src\n", "is not an absolute path"},
		{"DOCS_SOURCE=~other/doc\n", "is not an absolute path"},
		{"ENDPOINT=\"\"\n", "ENDPOINT: must not be empty"},
	}
	for _, tt := range tests {
		s := defaultSettings()
		err := s.parse(strings.NewReader(tt.input), "test.conf")
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("parse(%q) error = %v, want it to contain %q", tt.input, err, tt.want)
		}
	}
}

func TestConfigValue(t *testing.T) {
	for in, want := range map[string]string{
		"pkg":                     "pkg",
		"  pkg  ":                 "pkg",
		`"two words"`:             "two words",
		`'two words'`:             "two words",
		`"quoted" # comment`:      "quoted",
		"plain # comment":         "plain",
		"plain\t# comment":        "plain",
		"http://host/path#anchor": "http://host/path#anchor",
		`"has # inside"`:          "has # inside",
		`"unterminated`:           `"unterminated`,
		"":                        "",
	} {
		if got := configValue(in); got != want {
			t.Errorf("configValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// isolate points every configuration source at an empty temporary directory
// and returns that directory.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := systemConfigPath
	systemConfigPath = filepath.Join(dir, "system.conf")
	t.Cleanup(func() { systemConfigPath = old })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("BEASTIEPILOT_CONFIG", "")
	for _, key := range settingKeys {
		t.Setenv("BEASTIEPILOT_"+key, "")
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSettingsDefaults(t *testing.T) {
	isolate(t)
	s, files, err := loadSettings("")
	if err != nil || len(files) != 0 || s != defaultSettings() {
		t.Errorf("loadSettings = (%+v, %v, %v), want the defaults and no files", s, files, err)
	}
}

func TestLoadSettingsPrecedence(t *testing.T) {
	isolate(t)
	writeFile(t, systemConfigPath, "MODEL=from-system\nTIMEOUT=30s\nPACKAGE_MANAGER=ports\n")
	writeFile(t, userConfigPath(), "MODEL=from-user\nTIMEOUT=45s\n")
	t.Setenv("BEASTIEPILOT_MODEL", "from-env")

	s, files, err := loadSettings("")
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.Model != "from-env" {
		t.Errorf("Model = %q, want the environment to win", s.Model)
	}
	if s.Timeout != 45*time.Second {
		t.Errorf("Timeout = %s, want the user file to override the system file", s.Timeout)
	}
	if s.PackageManager != "ports" {
		t.Errorf("PackageManager = %q, want the system file's value to survive", s.PackageManager)
	}
	if len(files) != 2 || files[0] != systemConfigPath || files[1] != userConfigPath() {
		t.Errorf("files = %v, want system then user", files)
	}
}

func TestLoadSettingsExplicitFile(t *testing.T) {
	dir := isolate(t)
	writeFile(t, userConfigPath(), "MODEL=from-user\n")
	explicit := filepath.Join(dir, "mine.conf")
	writeFile(t, explicit, "VERBOSE=yes\n")

	s, files, err := loadSettings(explicit)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if !s.Verbose || s.Model != defaultModel || len(files) != 1 || files[0] != explicit {
		t.Errorf("got (%+v, %v); want only %s to be read", s, files, explicit)
	}

	// BEASTIEPILOT_CONFIG selects the file the same way.
	t.Setenv("BEASTIEPILOT_CONFIG", explicit)
	if s, _, err := loadSettings(""); err != nil || !s.Verbose || s.Model != defaultModel {
		t.Errorf("with BEASTIEPILOT_CONFIG: (%+v, %v)", s, err)
	}

	if _, _, err := loadSettings(filepath.Join(dir, "missing.conf")); err == nil {
		t.Error("a missing explicit file should be an error")
	}
}

func TestLoadSettingsReportsBadInput(t *testing.T) {
	isolate(t)
	t.Setenv("BEASTIEPILOT_TIMEOUT", "soon")
	if _, _, err := loadSettings(""); err == nil || !strings.Contains(err.Error(), "BEASTIEPILOT_TIMEOUT") {
		t.Errorf("error = %v, want it to name BEASTIEPILOT_TIMEOUT", err)
	}

	t.Setenv("BEASTIEPILOT_TIMEOUT", "")
	writeFile(t, userConfigPath(), "MANAGER_PACKAGE=pkg\n")
	if _, _, err := loadSettings(""); err == nil || !strings.Contains(err.Error(), "config:1: MANAGER_PACKAGE: unknown setting") {
		t.Errorf("error = %v, want it to point at the unknown key", err)
	}
}

func TestSampleConfigMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile("beastiepilot.conf.sample")
	if err != nil {
		t.Fatal(err)
	}
	// Uncommenting every setting in the sample must parse and leave the
	// defaults unchanged, and the sample must cover every key.
	var active []string
	for _, line := range strings.Split(string(data), "\n") {
		if key, _, ok := strings.Cut(strings.TrimPrefix(line, "#"), "="); ok && strings.HasPrefix(line, "#") && key == strings.ToUpper(key) && !strings.Contains(key, " ") {
			active = append(active, strings.TrimPrefix(line, "#"))
		}
	}
	s := defaultSettings()
	if err := s.parse(strings.NewReader(strings.Join(active, "\n")), "sample"); err != nil {
		t.Fatalf("sample does not parse: %v", err)
	}
	if s != defaultSettings() {
		t.Errorf("sample values %+v differ from the defaults %+v", s, defaultSettings())
	}
	if len(active) != len(settingKeys) {
		t.Errorf("sample documents %d settings, want all %d: %v", len(active), len(settingKeys), active)
	}
}

func TestTypeAndBannerValues(t *testing.T) {
	tests := []struct{ key, value, wantType, wantBanner string }{
		{"TYPE", "GUIDED", typeGuided, bannerFull},
		{"TYPE", "safe", typeSafe, bannerFull},
		{"BANNER", "full", typeSafe, bannerFull},
		{"BANNER", "Compact", typeSafe, bannerCompact},
		{"BANNER", "off", typeSafe, bannerOff},
		{"BANNER", "true", typeSafe, bannerFull},
		{"BANNER", "no", typeSafe, bannerOff},
	}
	for _, tt := range tests {
		s := defaultSettings()
		if err := s.set(tt.key, tt.value); err != nil {
			t.Errorf("set(%s, %q): %v", tt.key, tt.value, err)
		}
		if s.Type != tt.wantType || s.Banner != tt.wantBanner {
			t.Errorf("set(%s, %q) gave Type=%q Banner=%q, want %q and %q", tt.key, tt.value, s.Type, s.Banner, tt.wantType, tt.wantBanner)
		}
	}
	if def := defaultSettings(); def.Type != typeSafe || def.AutoRun != "" {
		t.Errorf("defaults are TYPE=%q AUTO_RUN=%q; a suggestion must not run unasked unless the user opts in", def.Type, def.AutoRun)
	}
}

func TestPathSettings(t *testing.T) {
	def := defaultSettings()
	if def.SourcePath != "/usr/src" || def.DocsSource != "/usr/share/doc" || def.RagSource != "" {
		t.Errorf("default paths are %q, %q and %q; want FreeBSD's locations and no RAG source", def.SourcePath, def.DocsSource, def.RagSource)
	}

	t.Setenv("HOME", "/home/beastie")
	for in, want := range map[string]string{
		"":                  "",
		"/usr/src":          "/usr/src",
		"/usr/src/":         "/usr/src",
		"/usr//share/./doc": "/usr/share/doc",
		"~":                 "/home/beastie",
		"~/books":           "/home/beastie/books",
		"/Mixed/Case":       "/Mixed/Case",
	} {
		s := defaultSettings()
		if err := s.set("RAG_SOURCE", in); err != nil || s.RagSource != want {
			t.Errorf("set(RAG_SOURCE, %q) gave (%q, %v), want %q", in, s.RagSource, err, want)
		}
	}

	// Several places, separated like the entries of PATH.
	for in, want := range map[string]string{
		"/usr/share/doc:/usr/local/share/doc/freebsd": "/usr/share/doc:/usr/local/share/doc/freebsd",
		"/a/:~/books::/a:/b/../c:":                    "/a:/home/beastie/books:/c",
		":":                                           "",
	} {
		s := defaultSettings()
		if err := s.set("DOCS_SOURCE", in); err != nil || s.DocsSource != want {
			t.Errorf("set(DOCS_SOURCE, %q) gave (%q, %v), want %q", in, s.DocsSource, err, want)
		}
	}
	for _, in := range []string{"docs", "./docs", "/a:docs", "~other/books"} {
		s := defaultSettings()
		if err := s.set("DOCS_SOURCE", in); err == nil {
			t.Errorf("set(DOCS_SOURCE, %q) accepted a relative path as %q", in, s.DocsSource)
		}
	}

	// Each key reaches its own field.
	s := defaultSettings()
	for key, value := range map[string]string{"SOURCE_PATH": "/a", "DOCS_SOURCE": "/b", "RAG_SOURCE": "/c"} {
		if err := s.set(key, value); err != nil {
			t.Fatalf("set(%s): %v", key, err)
		}
	}
	if s.SourcePath != "/a" || s.DocsSource != "/b" || s.RagSource != "/c" {
		t.Errorf("got %q, %q, %q; want /a, /b, /c", s.SourcePath, s.DocsSource, s.RagSource)
	}
}

// get is the inverse of set: what it returns for a key sets that key to the
// same value again. Saving settings to a file depends on it.
func TestSettingsGet(t *testing.T) {
	want := settings{
		Model: "qwen2.5-coder:7b", EmbedModel: "bge-m3", Endpoint: "http://10.0.0.5:11434/api/generate",
		Timeout: 90 * time.Second, Context: 16384, History: 0, Type: typeGuided,
		AutoRun: "df, zfs list, service * status", PackageManager: "ports",
		SourcePath: "/usr/src-14", DocsSource: "/usr/share/doc:/usr/local/share/doc/freebsd", RagSource: "/home/me/My Books",
		Banner: bannerCompact, Verbose: true,
	}
	got := defaultSettings()
	for _, key := range settingKeys {
		if want.get(key) == defaultSettings().get(key) {
			t.Errorf("this test leaves %s at its default, which proves nothing", key)
		}
		if err := got.set(key, want.get(key)); err != nil {
			t.Errorf("set(%s, %q): %v", key, want.get(key), err)
		}
	}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if v := want.get("NO_SUCH_KEY"); v != "" {
		t.Errorf("get of an unknown key gave %q", v)
	}
}

func TestPromptPreferences(t *testing.T) {
	s := defaultSettings()
	if got := s.promptPreferences(); got != "" {
		t.Errorf("default preferences = %q, want none", got)
	}
	s.PackageManager = "ports"
	if got := s.promptPreferences(); !strings.Contains(got, "/usr/ports") {
		t.Errorf("ports preferences = %q, want them to mention /usr/ports", got)
	}
}
