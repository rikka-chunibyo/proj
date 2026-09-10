package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeMounts map[string]bool

func (f fakeMounts) IsMount(path string) (bool, error) { return f[filepath.Clean(path)], nil }

func testApp(t *testing.T) (*App, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	active := filepath.Join(root, "active")
	mount := filepath.Join(root, "mnt")
	archive := filepath.Join(mount, "code")
	for _, dir := range []string{home, active, mount} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths := Paths{Home: home, ConfigFile: filepath.Join(root, "config", "proj", "config.toml"), RegistryFile: filepath.Join(root, "data", "proj", "projects.toml"), StateDir: filepath.Join(root, "state", "proj")}
	cfg := Config{Version: 1, Stores: map[string]StoreConfig{"active": {Root: active}, "archive": {Root: archive, Mount: mount}}}
	a := &App{Paths: paths, Config: cfg, Mounts: fakeMounts{mount: true}, In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, CWD: root, TTY: false}
	return a, root
}

func makeProject(t *testing.T, a *App, store, name string) string {
	t.Helper()
	root := filepath.Join(a.Config.Stores[store].Root, "projects", name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}
func writeFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
func outString(a *App) string { return a.Out.(*bytes.Buffer).String() }
func errString(a *App) string { return a.Err.(*bytes.Buffer).String() }
func resetOutput(a *App)      { a.Out.(*bytes.Buffer).Reset(); a.Err.(*bytes.Buffer).Reset() }

func TestDiscoverPaths(t *testing.T) {
	home := "/home/tester"
	p, err := discoverPaths(func(string) string { return "" }, home)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != "/home/tester/.config/proj/config.toml" || p.RegistryFile != "/home/tester/.local/share/proj/projects.toml" || p.StateDir != "/home/tester/.local/state/proj" {
		t.Fatalf("unexpected fallback paths: %+v", p)
	}
	env := map[string]string{"XDG_CONFIG_HOME": "/cfg", "XDG_DATA_HOME": "/data", "XDG_STATE_HOME": "/state"}
	p, err = discoverPaths(func(k string) string { return env[k] }, home)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != "/cfg/proj/config.toml" || p.RegistryFile != "/data/proj/projects.toml" || p.StateDir != "/state/proj" {
		t.Fatalf("unexpected XDG paths: %+v", p)
	}
	env["XDG_CONFIG_HOME"] = "relative"
	if _, err := discoverPaths(func(k string) string { return env[k] }, home); err == nil {
		t.Fatal("relative XDG path accepted")
	}
}

func TestConfigRoundTripAndMalformed(t *testing.T) {
	a, _ := testApp(t)
	text := configText(a.Config, a.Paths.Home)
	cfg, err := parseConfig(text, a.Paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Stores["active"].Root != a.Config.Stores["active"].Root {
		t.Fatalf("roundtrip: %+v", cfg)
	}
	if _, err := parseConfig("version = 99\n", a.Paths.Home); err == nil {
		t.Fatal("unsupported version accepted")
	}
	if _, err := parseConfig("version = 1\n[stores.active\n", a.Paths.Home); err == nil {
		t.Fatal("malformed config accepted")
	}
	if err := ensureConfig(a.Paths.ConfigFile, a.Config, a.Paths.Home); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a.Paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %o", info.Mode().Perm())
	}
}

func TestRegistryDeterministicAndValidation(t *testing.T) {
	r := Registry{Version: 1, Projects: []Project{{ID: "b", Name: "zeta", Store: "active", Tags: []string{"z", "a", "a"}, Languages: []string{"rust", "go"}, BuildSystems: []string{"cargo"}, GitKind: "none"}, {ID: "a", Name: "Matrix", Store: "archive", GitKind: "none"}}}
	text := registryText(r)
	if strings.Index(text, "name = \"Matrix\"") > strings.Index(text, "name = \"zeta\"") {
		t.Fatal("registry not sorted")
	}
	if !strings.Contains(text, `tags = ["a", "z"]`) || strings.Contains(text, `tags = ["a", "a"`) {
		t.Fatalf("tags not deduplicated:\n%s", text)
	}
	parsed, err := parseRegistry(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Projects) != 2 || parsed.Projects[0].Name != "Matrix" {
		t.Fatalf("roundtrip: %+v", parsed)
	}
	dup := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", GitKind: "none"}, {ID: "2", Name: "matrix", Store: "archive", GitKind: "none"}}}
	if err := validateRegistry(dup); err == nil {
		t.Fatal("case-insensitive duplicate accepted")
	}
	dup.Projects[1].Name = "Other"
	dup.Projects[1].ID = "1"
	if err := validateRegistry(dup); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	if _, err := parseRegistry("version = 1\n[[projects]]\nwat = 3\n"); err == nil {
		t.Fatal("malformed registry accepted")
	}
	badMeta := Registry{Version: 1, Projects: []Project{{ID: "x", Name: "Safe", Store: "active", Languages: []string{"../../escape"}, GitKind: "none"}}}
	if err := validateRegistry(badMeta); err == nil {
		t.Fatal("path-dangerous metadata accepted")
	}
}

func TestStableIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 36 || seen[id] {
			t.Fatalf("bad or duplicate ID %q", id)
		}
		seen[id] = true
	}
}

func TestAtomicRegistryAndLock(t *testing.T) {
	a, _ := testApp(t)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	got, err := loadRegistry(a.Paths.RegistryFile)
	if err != nil || len(got.Projects) != 1 {
		t.Fatalf("load: %+v %v", got, err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(a.Paths.RegistryFile), ".projects.toml.tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
	l1, err := acquireLock(a.Paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *FileLock, 1)
	go func() { l, _ := acquireLock(a.Paths.StateDir); acquired <- l }()
	select {
	case <-acquired:
		t.Fatal("second lock did not block")
	case <-time.After(50 * time.Millisecond):
	}
	_ = l1.Close()
	select {
	case l := <-acquired:
		_ = l.Close()
	case <-time.After(time.Second):
		t.Fatal("second lock did not proceed")
	}
}

func TestNameTagAndMountSafety(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "a\x00b", "a\nb"} {
		if validateProjectName(bad) == nil {
			t.Errorf("accepted name %q", bad)
		}
	}
	if err := validateProjectName("Unit 5 - Assembler programming"); err != nil {
		t.Fatal(err)
	}
	if err := validateProjectName(`legacy\name`); err != nil {
		t.Fatalf("existing Windows-problematic name should be warnable: %v", err)
	}
	if windowsNameProblem("CON") == "" || windowsNameProblem("bad.") == "" || windowsNameProblem("bad:name") == "" {
		t.Fatal("missed Windows-dangerous name")
	}
	if got, err := normalizeTag(" Logic  Analyzer "); err != nil || got != "logic-analyzer" {
		t.Fatalf("tag=%q err=%v", got, err)
	}
	for _, bad := range []string{"../x", "_bad", "bad/tag", "x!"} {
		if _, err := normalizeTag(bad); err == nil {
			t.Errorf("accepted tag %q", bad)
		}
	}
	mountInfo := "36 25 0:32 / /mnt/hdd rw,nosuid - virtiofs hdd rw\n37 25 0:33 / /with\\040space rw - ext4 /dev/x rw\n"
	ok, err := mountInfoContains(strings.NewReader(mountInfo), "/mnt/hdd")
	if err != nil || !ok {
		t.Fatalf("mount not found: %v", err)
	}
	ok, _ = mountInfoContains(strings.NewReader(mountInfo), "/mnt")
	if ok {
		t.Fatal("parent incorrectly treated as mount")
	}
	ok, _ = mountInfoContains(strings.NewReader(mountInfo), "/with space")
	if !ok {
		t.Fatal("escaped mount not decoded")
	}
}

func TestCanonicalResolution(t *testing.T) {
	a, _ := testApp(t)
	path := makeProject(t, a, "active", "Matrix")
	reg := Registry{Version: 1, Projects: []Project{{ID: "stable-id", Name: "Matrix", Store: "active", Tags: []string{"minecraft"}, Languages: []string{"java"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Matrix", "matrix", "MATRIX", "MaTrIx", "stable-id"} {
		p, _, err := resolveProject(reg, a.Config, name)
		if err != nil || p.Name != "Matrix" {
			t.Fatalf("resolve %q: %+v %v", name, p, err)
		}
	}
	if _, _, err := resolveProject(reg, a.Config, "Mattrix"); err == nil {
		t.Fatal("fuzzy direct resolution")
	}
	nested := filepath.Join(path, "src", "main")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _, err := resolveProject(reg, a.Config, nested)
	if err != nil || p.Name != "Matrix" {
		t.Fatalf("nested resolve: %v", err)
	}
	if _, err := reindexStore(a.Config, "active", reg, a.Mounts); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{filepath.Join(a.Config.Stores["active"].Root, "by-tag", "minecraft", "Matrix"), filepath.Join(a.Config.Stores["active"].Root, "by-language", "java", "Matrix")} {
		p, _, err := resolveProject(reg, a.Config, view)
		if err != nil || p.Name != "Matrix" {
			t.Fatalf("view resolve %s: %v", view, err)
		}
	}
}

func TestValidationRejectsCanonicalSymlinkAndProtectedPaths(t *testing.T) {
	a, root := testApp(t)
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(a.Config.Stores["active"].Root, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(projects, "Link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRealDirectory(link); err == nil {
		t.Fatal("canonical symlink accepted")
	}
	for _, path := range []string{"/", a.Paths.Home, a.Config.Stores["active"].Root, projects} {
		if err := validateSafeSource(path, a.Paths, a.Config); err == nil {
			t.Errorf("protected source accepted: %s", path)
		}
	}
}

func TestErrorsHelper(t *testing.T) {
	if !errors.Is(os.ErrNotExist, os.ErrNotExist) {
		t.Fatal("unreachable")
	}
}
