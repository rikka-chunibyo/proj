package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Project struct {
	ID           string
	Name         string
	Store        string
	Languages    []string
	BuildSystems []string
	Tags         []string
	Git          bool
	GitKind      string
}

type Registry struct {
	Version  int
	Projects []Project
}

func emptyRegistry() Registry { return Registry{Version: registrySchemaVersion} }

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func canonicalPath(cfg Config, p Project) (string, error) {
	store, ok := cfg.Stores[p.Store]
	if !ok {
		return "", fmt.Errorf("project %q refers to unknown store %q", p.Name, p.Store)
	}
	if err := validateProjectName(p.Name); err != nil {
		return "", fmt.Errorf("project %q has invalid name: %w", p.Name, err)
	}
	return filepath.Join(store.Backing, p.Name), nil
}

func logicalPath(cfg Config, p Project) (string, error) {
	if p.Store == "active" {
		return canonicalPath(cfg, p)
	}
	if p.Store != "archive" {
		return "", fmt.Errorf("project %q refers to unknown store %q", p.Name, p.Store)
	}
	if err := validateProjectName(p.Name); err != nil {
		return "", err
	}
	return filepath.Join(cfg.Stores["archive"].WorkspacePath, p.Name), nil
}

func normalizeRegistry(reg *Registry) {
	for i := range reg.Projects {
		reg.Projects[i].Tags = sortedUnique(reg.Projects[i].Tags)
		reg.Projects[i].Languages = sortedUnique(reg.Projects[i].Languages)
		reg.Projects[i].BuildSystems = sortedUnique(reg.Projects[i].BuildSystems)
		if !reg.Projects[i].Git && reg.Projects[i].GitKind == "" {
			reg.Projects[i].GitKind = "none"
		}
	}
	sort.Slice(reg.Projects, func(i, j int) bool {
		a, b := strings.ToLower(reg.Projects[i].Name), strings.ToLower(reg.Projects[j].Name)
		if a == b {
			return reg.Projects[i].ID < reg.Projects[j].ID
		}
		return a < b
	})
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = true
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateRegistry(reg Registry) error {
	if reg.Version != registrySchemaVersion {
		return fmt.Errorf("unsupported registry version %d (supported: %d)", reg.Version, registrySchemaVersion)
	}
	ids := map[string]string{}
	var priorNames []string
	for _, p := range reg.Projects {
		if p.ID == "" {
			return fmt.Errorf("project %q has an empty stable ID", p.Name)
		}
		if prior, ok := ids[p.ID]; ok {
			return fmt.Errorf("duplicate stable ID %q used by %q and %q", p.ID, prior, p.Name)
		}
		ids[p.ID] = p.Name
		if err := validateProjectName(p.Name); err != nil {
			return fmt.Errorf("project %q: %w", p.Name, err)
		}
		for _, prior := range priorNames {
			if strings.EqualFold(prior, p.Name) {
				return fmt.Errorf("case-insensitive project-name conflict between %q and %q", prior, p.Name)
			}
		}
		priorNames = append(priorNames, p.Name)
		if p.Store != "active" && p.Store != "archive" {
			return fmt.Errorf("project %q has invalid store %q", p.Name, p.Store)
		}
		for _, tag := range p.Tags {
			if normalized, err := normalizeTag(tag); err != nil || normalized != tag {
				return fmt.Errorf("project %q has non-canonical tag %q", p.Name, tag)
			}
		}
		for _, language := range p.Languages {
			if err := validateMetadataID("language", language); err != nil {
				return fmt.Errorf("project %q: %w", p.Name, err)
			}
		}
		for _, build := range p.BuildSystems {
			if err := validateMetadataID("build system", build); err != nil {
				return fmt.Errorf("project %q: %w", p.Name, err)
			}
		}
	}
	return nil
}

func registryText(reg Registry) string {
	normalizeRegistry(&reg)
	var b strings.Builder
	fmt.Fprintf(&b, "version = %d\n", reg.Version)
	for _, p := range reg.Projects {
		b.WriteString("\n[[projects]]\n")
		fmt.Fprintf(&b, "id = %s\n", strconv.Quote(p.ID))
		fmt.Fprintf(&b, "name = %s\n", strconv.Quote(p.Name))
		fmt.Fprintf(&b, "store = %s\n", strconv.Quote(p.Store))
		fmt.Fprintf(&b, "languages = %s\n", encodeStringArray(p.Languages))
		fmt.Fprintf(&b, "build_systems = %s\n", encodeStringArray(p.BuildSystems))
		fmt.Fprintf(&b, "tags = %s\n", encodeStringArray(p.Tags))
		fmt.Fprintf(&b, "git = %t\n", p.Git)
		fmt.Fprintf(&b, "git_kind = %s\n", strconv.Quote(p.GitKind))
	}
	return b.String()
}

func encodeStringArray(values []string) string {
	values = sortedUnique(values)
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Quote(value)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func parseStringArray(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, errors.New("expected an array of quoted strings")
	}
	raw = strings.TrimSpace(raw[1 : len(raw)-1])
	if raw == "" {
		return []string{}, nil
	}
	var values []string
	for len(raw) > 0 {
		if raw[0] != '"' {
			return nil, errors.New("expected quoted string in array")
		}
		escaped := false
		end := -1
		for i := 1; i < len(raw); i++ {
			if escaped {
				escaped = false
				continue
			}
			if raw[i] == '\\' {
				escaped = true
			} else if raw[i] == '"' {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, errors.New("unterminated string in array")
		}
		value, err := strconv.Unquote(raw[:end+1])
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		raw = strings.TrimSpace(raw[end+1:])
		if raw == "" {
			break
		}
		if raw[0] != ',' {
			return nil, errors.New("expected comma in array")
		}
		raw = strings.TrimSpace(raw[1:])
	}
	return values, nil
}

func loadRegistry(path string) (Registry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyRegistry(), nil
	}
	if err != nil {
		return Registry{}, fmt.Errorf("read registry: %w", err)
	}
	return parseRegistry(string(b))
}

func parseRegistry(text string) (Registry, error) {
	reg := Registry{}
	var current *Project
	scanner := bufio.NewScanner(strings.NewReader(text))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" {
			continue
		}
		if line == "[[projects]]" {
			reg.Projects = append(reg.Projects, Project{})
			current = &reg.Projects[len(reg.Projects)-1]
			continue
		}
		key, raw, ok := splitAssignment(line)
		if !ok {
			return Registry{}, fmt.Errorf("registry line %d: expected key = value", lineNo)
		}
		if current == nil {
			if key != "version" {
				return Registry{}, fmt.Errorf("registry line %d: unexpected key %q", lineNo, key)
			}
			v, err := strconv.Atoi(raw)
			if err != nil {
				return Registry{}, fmt.Errorf("registry line %d: invalid version", lineNo)
			}
			reg.Version = v
			continue
		}
		var err error
		switch key {
		case "id", "name", "store", "git_kind":
			var value string
			value, err = strconv.Unquote(raw)
			if key == "id" {
				current.ID = value
			}
			if key == "name" {
				current.Name = value
			}
			if key == "store" {
				current.Store = value
			}
			if key == "git_kind" {
				current.GitKind = value
			}
		case "languages":
			current.Languages, err = parseStringArray(raw)
		case "build_systems":
			current.BuildSystems, err = parseStringArray(raw)
		case "tags":
			current.Tags, err = parseStringArray(raw)
		case "git":
			current.Git, err = strconv.ParseBool(raw)
		default:
			return Registry{}, fmt.Errorf("registry line %d: unknown key %q", lineNo, key)
		}
		if err != nil {
			return Registry{}, fmt.Errorf("registry line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Registry{}, err
	}
	normalizeRegistry(&reg)
	if err := validateRegistry(reg); err != nil {
		return Registry{}, err
	}
	return reg, nil
}

func saveRegistry(path string, reg Registry) error {
	normalizeRegistry(&reg)
	if err := validateRegistry(reg); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicWrite(path, []byte(registryText(reg)), 0o600)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	if err != nil {
		return fmt.Errorf("sync containing directory: %w", err)
	}
	ok = true
	return nil
}

type FileLock struct{ file *os.File }

func acquireLock(stateDir string) (*FileLock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "registry.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &FileLock{file: f}, nil
}

func (l *FileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err1 := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err2 := l.file.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
