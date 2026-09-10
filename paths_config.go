package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const configSchemaVersion = 2
const registrySchemaVersion = 1

type Paths struct{ Home, ConfigFile, RegistryFile, StateDir string }
type StoreConfig struct{ Backing, Mount, WorkspacePath string }
type ViewConfig struct{ Tags, Languages string }
type Config struct {
	Version       int
	WorkspaceRoot string
	Stores        map[string]StoreConfig
	Views         ViewConfig
}
type LegacyConfigError struct{ Message string }

func (e *LegacyConfigError) Error() string { return e.Message }

func discoverPaths(getenv func(string) string, home string) (Paths, error) {
	if home == "" {
		return Paths{}, errors.New("cannot determine the current user's home directory")
	}
	base := func(env, fallback string) string {
		if value := getenv(env); value != "" {
			if filepath.IsAbs(value) {
				return filepath.Clean(value)
			}
			return ""
		}
		return filepath.Join(home, fallback)
	}
	config, data, state := base("XDG_CONFIG_HOME", ".config"), base("XDG_DATA_HOME", filepath.Join(".local", "share")), base("XDG_STATE_HOME", filepath.Join(".local", "state"))
	if config == "" || data == "" || state == "" {
		return Paths{}, errors.New("XDG_CONFIG_HOME, XDG_DATA_HOME, and XDG_STATE_HOME must be absolute when set")
	}
	return Paths{filepath.Clean(home), filepath.Join(config, "proj", "config.toml"), filepath.Join(data, "proj", "projects.toml"), filepath.Join(state, "proj")}, nil
}

func defaultConfig(home string) Config {
	workspace := filepath.Join(home, "code")
	return Config{Version: configSchemaVersion, WorkspaceRoot: workspace, Stores: map[string]StoreConfig{"active": {Backing: filepath.Join(workspace, "projects")}, "archive": {Backing: "/mnt/hdd/archive/code", Mount: "/mnt/hdd", WorkspacePath: filepath.Join(workspace, "archive")}}, Views: ViewConfig{Tags: filepath.Join(workspace, "by-tag"), Languages: filepath.Join(workspace, "by-language")}}
}

func expandHome(path, home string) (string, error) {
	switch {
	case path == "~":
		return home, nil
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:]), nil
	case strings.HasPrefix(path, "~"):
		return "", fmt.Errorf("unsupported home expansion %q; only ~ and ~/... are accepted", path)
	case filepath.IsAbs(path):
		return filepath.Clean(path), nil
	default:
		return "", fmt.Errorf("configured path %q must be absolute or start with ~/", path)
	}
}

func loadConfig(path, home string) (Config, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(home), false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("read config: %w", err)
	}
	cfg, err := parseConfig(string(b), home)
	return cfg, true, err
}

func parseConfig(text, home string) (Config, error) {
	cfg := Config{Stores: map[string]StoreConfig{}}
	section := ""
	legacyRootKey := false
	scanner := bufio.NewScanner(strings.NewReader(text))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return Config{}, fmt.Errorf("config line %d: malformed section", lineNo)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			switch section {
			case "workspace", "stores.active", "stores.archive", "views":
			default:
				return Config{}, fmt.Errorf("config line %d: unknown section %q", lineNo, section)
			}
			continue
		}
		key, raw, ok := splitAssignment(line)
		if !ok {
			return Config{}, fmt.Errorf("config line %d: expected key = value", lineNo)
		}
		if section == "" && key == "version" {
			v, err := strconv.Atoi(raw)
			if err != nil {
				return Config{}, fmt.Errorf("config line %d: invalid version", lineNo)
			}
			cfg.Version = v
			continue
		}
		if section == "" {
			return Config{}, fmt.Errorf("config line %d: unknown key %q", lineNo, key)
		}
		value, err := strconv.Unquote(raw)
		if err != nil {
			return Config{}, fmt.Errorf("config line %d: %w", lineNo, err)
		}
		value, err = expandHome(value, home)
		if err != nil {
			return Config{}, fmt.Errorf("config line %d: %w", lineNo, err)
		}
		switch section {
		case "workspace":
			if key != "root" {
				return Config{}, fmt.Errorf("config line %d: unknown key %q", lineNo, key)
			}
			cfg.WorkspaceRoot = value
		case "stores.active", "stores.archive":
			name := strings.TrimPrefix(section, "stores.")
			store := cfg.Stores[name]
			switch key {
			case "backing":
				store.Backing = value
			case "mount":
				store.Mount = value
			case "workspace_path":
				store.WorkspacePath = value
			case "root":
				legacyRootKey = true
			default:
				return Config{}, fmt.Errorf("config line %d: unknown key %q", lineNo, key)
			}
			cfg.Stores[name] = store
		case "views":
			switch key {
			case "tags":
				cfg.Views.Tags = value
			case "languages":
				cfg.Views.Languages = value
			default:
				return Config{}, fmt.Errorf("config line %d: unknown key %q", lineNo, key)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}
	if cfg.Version == 1 || legacyRootKey {
		return Config{}, &LegacyConfigError{Message: "legacy proj config layout detected; registry data was not changed. Install the schema-v2 config after completing the documented archive/bind-mount setup, then run `proj doctor`"}
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.Version != configSchemaVersion {
		return fmt.Errorf("unsupported config version %d (supported: %d)", cfg.Version, configSchemaVersion)
	}
	paths := map[string]string{"workspace root": cfg.WorkspaceRoot, "active backing": cfg.Stores["active"].Backing, "archive backing": cfg.Stores["archive"].Backing, "archive workspace path": cfg.Stores["archive"].WorkspacePath, "tag view": cfg.Views.Tags, "language view": cfg.Views.Languages}
	for label, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path", label)
		}
		if filepath.Clean(path) == string(filepath.Separator) {
			return fmt.Errorf("%s cannot be /", label)
		}
	}
	archive := cfg.Stores["archive"]
	if archive.Mount == "" || !filepath.IsAbs(archive.Mount) {
		return errors.New("archive store must have an absolute mount path")
	}
	if filepath.Clean(archive.Mount) == string(filepath.Separator) {
		return errors.New("archive mount cannot be /")
	}
	if !pathWithin(archive.Backing, archive.Mount) || filepath.Clean(archive.Backing) == filepath.Clean(archive.Mount) {
		return errors.New("archive backing must be a child of the configured archive mount")
	}
	if pathWithin(archive.Backing, cfg.WorkspaceRoot) {
		return errors.New("physical archive backing must be outside the logical workspace root")
	}
	for label, path := range map[string]string{"active backing": cfg.Stores["active"].Backing, "archive workspace path": archive.WorkspacePath, "tag view": cfg.Views.Tags, "language view": cfg.Views.Languages} {
		if !pathWithin(path, cfg.WorkspaceRoot) || filepath.Clean(path) == filepath.Clean(cfg.WorkspaceRoot) {
			return fmt.Errorf("%s must be a child of the workspace root", label)
		}
	}
	all := []string{cfg.Stores["active"].Backing, archive.Backing, archive.WorkspacePath, cfg.Views.Tags, cfg.Views.Languages}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if pathWithin(all[i], all[j]) || pathWithin(all[j], all[i]) {
				return fmt.Errorf("configured managed paths must not overlap: %s and %s", all[i], all[j])
			}
		}
	}
	return nil
}

func configText(cfg Config, home string) string {
	display := func(path string) string {
		if path == home {
			return "~"
		}
		if pathWithin(path, home) {
			return "~/" + filepath.ToSlash(strings.TrimPrefix(path, home+string(filepath.Separator)))
		}
		return path
	}
	return fmt.Sprintf("version = %d\n\n[workspace]\nroot = %s\n\n[stores.active]\nbacking = %s\n\n[stores.archive]\nbacking = %s\nmount = %s\nworkspace_path = %s\n\n[views]\ntags = %s\nlanguages = %s\n", cfg.Version, strconv.Quote(display(cfg.WorkspaceRoot)), strconv.Quote(display(cfg.Stores["active"].Backing)), strconv.Quote(display(cfg.Stores["archive"].Backing)), strconv.Quote(display(cfg.Stores["archive"].Mount)), strconv.Quote(display(cfg.Stores["archive"].WorkspacePath)), strconv.Quote(display(cfg.Views.Tags)), strconv.Quote(display(cfg.Views.Languages)))
}
func ensureConfig(path string, cfg Config, home string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicWrite(path, []byte(configText(cfg, home)), 0o600)
}
func stripComment(s string) string {
	inString, escaped := false, false
	for i, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
		} else if r == '#' && !inString {
			return s[:i]
		}
	}
	return s
}
func splitAssignment(line string) (string, string, bool) {
	idx := strings.IndexByte(line, '=')
	if idx < 1 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}
