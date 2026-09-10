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

const schemaVersion = 1

type Paths struct {
	Home         string
	ConfigFile   string
	RegistryFile string
	StateDir     string
}

type StoreConfig struct {
	Root  string
	Mount string
}

type Config struct {
	Version int
	Stores  map[string]StoreConfig
}

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
	config := base("XDG_CONFIG_HOME", ".config")
	data := base("XDG_DATA_HOME", filepath.Join(".local", "share"))
	state := base("XDG_STATE_HOME", filepath.Join(".local", "state"))
	if config == "" || data == "" || state == "" {
		return Paths{}, errors.New("XDG_CONFIG_HOME, XDG_DATA_HOME, and XDG_STATE_HOME must be absolute when set")
	}
	return Paths{
		Home:         filepath.Clean(home),
		ConfigFile:   filepath.Join(config, "proj", "config.toml"),
		RegistryFile: filepath.Join(data, "proj", "projects.toml"),
		StateDir:     filepath.Join(state, "proj"),
	}, nil
}

func defaultConfig(home string) Config {
	return Config{Version: schemaVersion, Stores: map[string]StoreConfig{
		"active":  {Root: filepath.Join(home, "code")},
		"archive": {Root: "/mnt/hdd/code", Mount: "/mnt/hdd"},
	}}
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
		return "", fmt.Errorf("store path %q must be absolute or start with ~/", path)
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
			if section != "stores.active" && section != "stores.archive" {
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
		storeName := strings.TrimPrefix(section, "stores.")
		store := cfg.Stores[storeName]
		switch key {
		case "root":
			store.Root, err = expandHome(value, home)
		case "mount":
			store.Mount, err = expandHome(value, home)
		default:
			return Config{}, fmt.Errorf("config line %d: unknown key %q", lineNo, key)
		}
		if err != nil {
			return Config{}, fmt.Errorf("config line %d: %w", lineNo, err)
		}
		cfg.Stores[storeName] = store
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.Version != schemaVersion {
		return fmt.Errorf("unsupported config version %d (supported: %d)", cfg.Version, schemaVersion)
	}
	for _, name := range []string{"active", "archive"} {
		store, ok := cfg.Stores[name]
		if !ok || store.Root == "" || !filepath.IsAbs(store.Root) {
			return fmt.Errorf("store %q must have an absolute root", name)
		}
		if filepath.Clean(store.Root) == string(filepath.Separator) {
			return fmt.Errorf("store %q root cannot be /", name)
		}
	}
	archive := cfg.Stores["archive"]
	if archive.Mount == "" || !filepath.IsAbs(archive.Mount) {
		return errors.New("archive store must have an absolute mount path")
	}
	if !pathWithin(archive.Root, archive.Mount) || filepath.Clean(archive.Root) == filepath.Clean(archive.Mount) {
		return errors.New("archive root must be a child of the configured archive mount")
	}
	if filepath.Clean(cfg.Stores["active"].Root) == filepath.Clean(archive.Root) {
		return errors.New("active and archive roots must differ")
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
	return fmt.Sprintf("version = %d\n\n[stores.active]\nroot = %s\n\n[stores.archive]\nroot = %s\nmount = %s\n",
		cfg.Version,
		strconv.Quote(display(cfg.Stores["active"].Root)),
		strconv.Quote(display(cfg.Stores["archive"].Root)),
		strconv.Quote(display(cfg.Stores["archive"].Mount)))
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
	inString := false
	escaped := false
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
