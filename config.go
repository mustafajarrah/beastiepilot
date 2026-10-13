package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Where FreeBSD keeps its source tree and its documentation: the src
// distribution set is installed to /usr/src, and hier(7) gives /usr/share/doc
// for the documentation.
const (
	defaultSourcePath = "/usr/src"
	defaultDocsSource = "/usr/share/doc"
)

// systemConfigPath is the machine-wide configuration file, following the
// FreeBSD convention for software installed outside the base system. It is a
// variable so that tests can point it elsewhere.
var systemConfigPath = "/usr/local/etc/beastiepilot.conf"

// The values of TYPE, which decides what happens to a suggestion.
const (
	typeGuided = "guided" // explain the command, then ask before running it
	typeSafe   = "safe"   // ask before running it, unless AUTO_RUN covers it
)

// The values of BANNER.
const (
	bannerFull    = "full"    // the animated sprite, when the terminal allows
	bannerCompact = "compact" // the four-line mascot
	bannerOff     = "off"
)

// settings holds everything the user can configure. Each field has a KEY in
// the configuration file, a BEASTIEPILOT_KEY environment variable and a
// command-line flag; later sources override earlier ones.
type settings struct {
	Model          string        // MODEL
	EmbedModel     string        // EMBED_MODEL: the model that embeds text for the index
	Endpoint       string        // ENDPOINT
	Timeout        time.Duration // TIMEOUT
	Context        int           // CONTEXT: the context window asked of Ollama, in tokens
	History        int           // HISTORY: how many earlier requests go along with a new one
	Type           string        // TYPE: typeGuided or typeSafe
	AutoRun        string        // AUTO_RUN: commands safe mode runs unasked, as parseAutoRun returns them
	PackageManager string        // PACKAGE_MANAGER: "pkg" or "ports"
	SourcePath     string        // SOURCE_PATH: the FreeBSD source tree, or "" for none
	DocsSource     string        // DOCS_SOURCE: the system documentation, or "" for none
	RagSource      string        // RAG_SOURCE: the user's own reference files, or "" for none; each of the three as cleanPaths returns it
	Banner         string        // BANNER: bannerFull, bannerCompact or bannerOff
	Verbose        bool          // VERBOSE
}

// settingKeys lists the configuration keys in the order they are documented.
var settingKeys = []string{"MODEL", "EMBED_MODEL", "ENDPOINT", "TIMEOUT", "CONTEXT", "HISTORY", "TYPE", "AUTO_RUN",
	"PACKAGE_MANAGER", "SOURCE_PATH", "DOCS_SOURCE", "RAG_SOURCE", "BANNER", "VERBOSE"}

func defaultSettings() settings {
	return settings{
		Model:          defaultModel,
		EmbedModel:     defaultEmbedModel,
		Endpoint:       defaultEndpoint,
		Timeout:        defaultTimeout,
		Context:        defaultContext,
		History:        defaultHistory,
		Type:           typeSafe,
		PackageManager: "pkg",
		SourcePath:     defaultSourcePath,
		DocsSource:     defaultDocsSource,
		Banner:         bannerFull,
	}
}

// set assigns the setting named key from its textual form.
func (s *settings) set(key, value string) error {
	switch key {
	case "MODEL":
		if value == "" {
			return errors.New("must not be empty")
		}
		s.Model = value
	case "EMBED_MODEL":
		if value == "" {
			return errors.New("must not be empty")
		}
		s.EmbedModel = value
	case "ENDPOINT":
		if value == "" {
			return errors.New("must not be empty")
		}
		s.Endpoint = value
	case "TIMEOUT":
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("%q is not a positive duration such as 90s or 2m", value)
		}
		s.Timeout = d
	case "CONTEXT":
		n, err := strconv.Atoi(value)
		if err != nil || n < minContext || n > maxContext {
			return fmt.Errorf("%q is not a number of tokens from %d to %d", value, minContext, maxContext)
		}
		s.Context = n
	case "HISTORY":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > maxHistory {
			return fmt.Errorf("%q is not a number of requests from 0 to %d", value, maxHistory)
		}
		s.History = n
	case "PACKAGE_MANAGER":
		v := strings.ToLower(value)
		if v != "pkg" && v != "ports" {
			return fmt.Errorf("%q is not a package manager (use pkg or ports)", value)
		}
		s.PackageManager = v
	case "TYPE":
		v := strings.ToLower(value)
		if v != typeGuided && v != typeSafe {
			return fmt.Errorf("%q is not a type (use guided or safe)", value)
		}
		s.Type = v
	case "AUTO_RUN":
		list, err := parseAutoRun(value)
		if err != nil {
			return err
		}
		s.AutoRun = list
	case "SOURCE_PATH", "DOCS_SOURCE", "RAG_SOURCE":
		p, err := cleanPaths(value)
		if err != nil {
			return err
		}
		switch key {
		case "SOURCE_PATH":
			s.SourcePath = p
		case "DOCS_SOURCE":
			s.DocsSource = p
		default:
			s.RagSource = p
		}
	case "BANNER":
		switch v := strings.ToLower(value); v {
		case bannerFull, bannerCompact, bannerOff:
			s.Banner = v
		default:
			// Plain booleans keep working: true is the full banner.
			on, err := parseBool(value)
			if err != nil {
				return fmt.Errorf("%q is not a banner style (use full, compact or off)", value)
			}
			s.Banner = bannerOff
			if on {
				s.Banner = bannerFull
			}
		}
	case "VERBOSE":
		b, err := parseBool(value)
		if err != nil {
			return err
		}
		s.Verbose = b
	default:
		return fmt.Errorf("unknown setting (known settings: %s)", strings.Join(settingKeys, ", "))
	}
	return nil
}

// get returns the setting named key in the textual form that set accepts, or
// "" for a key that does not exist.
func (s settings) get(key string) string {
	switch key {
	case "MODEL":
		return s.Model
	case "EMBED_MODEL":
		return s.EmbedModel
	case "ENDPOINT":
		return s.Endpoint
	case "TIMEOUT":
		return s.Timeout.String()
	case "CONTEXT":
		return strconv.Itoa(s.Context)
	case "HISTORY":
		return strconv.Itoa(s.History)
	case "TYPE":
		return s.Type
	case "AUTO_RUN":
		return s.AutoRun
	case "PACKAGE_MANAGER":
		return s.PackageManager
	case "SOURCE_PATH":
		return s.SourcePath
	case "DOCS_SOURCE":
		return s.DocsSource
	case "RAG_SOURCE":
		return s.RagSource
	case "BANNER":
		return s.Banner
	case "VERBOSE":
		return strconv.FormatBool(s.Verbose)
	}
	return ""
}

// cleanPaths validates a setting that names places to read and returns it in
// canonical form. The value is one path or several separated by ":", as in
// PATH; an empty value means none.
func cleanPaths(value string) (string, error) {
	var paths []string
	for _, p := range filepath.SplitList(value) {
		p, err := cleanPath(p)
		if err != nil {
			return "", err
		}
		if p != "" {
			paths = appendUnique(paths, p)
		}
	}
	return strings.Join(paths, string(filepath.ListSeparator)), nil
}

// cleanPath validates one path of such a setting and returns it in canonical
// form. A leading "~" is the user's home directory. Relative paths are
// refused, because what they name would depend on the directory the program
// was started from. Whether the path exists is not checked here: the source
// tree, for one, is optional.
func cleanPath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand %q: %w", value, err)
		}
		value = filepath.Join(home, value[1:])
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%q is not an absolute path", value)
	}
	return filepath.Clean(value), nil
}

// parseBool accepts the spellings people use in rc.conf and elsewhere.
func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean (use true or false)", value)
}

// loadSettings builds the settings from the defaults, the configuration
// files and the environment, in increasing order of precedence. It also
// returns the files it read.
//
// When explicit (or BEASTIEPILOT_CONFIG) names a file, only that file is read
// and it must exist. Otherwise the system-wide and per-user files are read if
// present. A file in the working directory is never consulted: a directory
// someone else prepared should not be able to redirect requests.
func loadSettings(explicit string) (settings, []string, error) {
	s := defaultSettings()

	paths, required := []string{systemConfigPath, userConfigPath()}, false
	if explicit == "" {
		explicit = os.Getenv("BEASTIEPILOT_CONFIG")
	}
	if explicit != "" {
		paths, required = []string{explicit}, true
	}

	var loaded []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) && !required {
			continue
		}
		if err != nil {
			return s, nil, fmt.Errorf("configuration file: %w", err)
		}
		err = s.parse(f, p)
		f.Close()
		if err != nil {
			return s, nil, err
		}
		loaded = append(loaded, p)
	}

	for _, key := range settingKeys {
		if v := os.Getenv("BEASTIEPILOT_" + key); v != "" {
			if err := s.set(key, v); err != nil {
				return s, nil, fmt.Errorf("BEASTIEPILOT_%s: %w", key, err)
			}
		}
	}
	return s, loaded, nil
}

// userConfigPath returns the per-user configuration file, honoring
// XDG_CONFIG_HOME, or "" if the home directory is unknown.
func userConfigPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "beastiepilot", "config")
}

// parse reads KEY=value lines from r into s. Blank lines and lines starting
// with "#" are ignored and keys are case-insensitive. name identifies the
// source in error messages.
func (s *settings) parse(r io.Reader, name string) error {
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=value", name, n)
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		if err := s.set(key, configValue(raw)); err != nil {
			return fmt.Errorf("%s:%d: %s: %w", name, n, key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// configValue extracts the value from the right-hand side of a KEY=value
// line. Surrounding single or double quotes are removed, as in rc.conf; on an
// unquoted value a trailing "# comment" is dropped.
func configValue(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : 1+end]
		}
	}
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// promptPreferences turns the settings that shape the suggestion itself into
// instructions appended to the system prompt.
func (s settings) promptPreferences() string {
	if s.PackageManager == "ports" {
		return "\n\nUSER PREFERENCES (these override the defaults above):\n" +
			"- Install third-party software by building it from the ports tree, for example: make -C /usr/ports/www/nginx install clean. Do not use pkg install. pkg is still fine for queries such as pkg info.\n"
	}
	return ""
}
