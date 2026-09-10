package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hddOnlyFakeMounts(cfg Config, options ...string) fakeMounts {
	if len(options) == 0 {
		options = []string{"rw"}
	}
	archive := cfg.Stores["archive"]
	return fakeMounts{records: []MountRecord{{
		Device: "8:1", Root: "/", MountPoint: archive.Mount,
		FSType: "virtiofs", Options: options,
	}}}
}

func TestGlobalViewsCombineActiveAndArchive(t *testing.T) {
	a, _ := testApp(t)
	makeProject(t, a, "active", "Matrix")
	makeProject(t, a, "archive", "OldPlugin")
	reg := Registry{Version: 1, Projects: []Project{
		{ID: "1", Name: "Matrix", Store: "active", Tags: []string{"minecraft"}, Languages: []string{"java"}, GitKind: "none"},
		{ID: "2", Name: "OldPlugin", Store: "archive", Tags: []string{"minecraft"}, Languages: []string{"java"}, GitKind: "none"},
	}}
	if _, err := reindexViews(a.Config, reg); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		filepath.Join(a.Config.Views.Tags, "minecraft", "Matrix"):    "../../projects/Matrix",
		filepath.Join(a.Config.Views.Tags, "minecraft", "OldPlugin"): "../../archive/OldPlugin",
		filepath.Join(a.Config.Views.Languages, "java", "Matrix"):    "../../projects/Matrix",
		filepath.Join(a.Config.Views.Languages, "java", "OldPlugin"): "../../archive/OldPlugin",
	}
	for path, want := range checks {
		got, err := os.Readlink(path)
		if err != nil || got != want {
			t.Fatalf("%s -> %q, want %q (err %v)", path, got, want, err)
		}
	}
	for _, unwanted := range []string{
		filepath.Join(a.Config.Stores["archive"].Backing, "by-tag"),
		filepath.Join(a.Config.Stores["archive"].Backing, "by-language"),
	} {
		if _, err := os.Lstat(unwanted); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("archive-local generated tree exists: %s", unwanted)
		}
	}
}

func TestReindexOfflineKeepsArchivedDanglingViews(t *testing.T) {
	a, _ := testApp(t)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Cold", Store: "archive", Tags: []string{"legacy"}, Languages: []string{"java"}, GitKind: "none"}}}
	a.Mounts = fakeMounts{}
	if _, err := reindexViews(a.Config, reg); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(a.Config.Views.Tags, "legacy", "Cold")
	if target, err := os.Readlink(link); err != nil || target != "../../archive/Cold" {
		t.Fatalf("offline archived link: %q, %v", target, err)
	}
	if _, err := os.Stat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected deliberately dangling link, got %v", err)
	}
	if _, err := reindexViews(a.Config, reg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("registered offline archived link was removed", err)
	}
}

func TestArchiveRestoreRetargetGlobalViews(t *testing.T) {
	a, _ := testApp(t)
	source := makeProject(t, a, "active", "Rune")
	writeFile(t, filepath.Join(source, "main.rs"), "fn main() {}", 0o644)
	p := Project{ID: "1", Name: "Rune", Store: "active", Tags: []string{"tooling"}, Languages: []string{"rust"}, GitKind: "none"}
	if err := saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{p}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reindexViews(a.Config, Registry{Version: 1, Projects: []Project{p}}); err != nil {
		t.Fatal(err)
	}
	a.RelocationHooks.ForceCopy = true
	if err := a.Run([]string{"archive", "Rune", "--yes"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(a.Config.Views.Tags, "tooling", "Rune")
	if got, _ := os.Readlink(link); got != "../../archive/Rune" {
		t.Fatalf("archive target %q", got)
	}
	// Restore reads the physical source and remains a recovery operation even
	// if the workspace archive symlink is absent.
	a.Mounts = hddOnlyFakeMounts(a.Config)
	if err := os.Remove(a.Config.Stores["archive"].WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"restore", "Rune", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != "../../projects/Rune" {
		t.Fatalf("restore target %q", got)
	}
}

func TestArchiveWritesRequireHealthyWorkspaceSymlink(t *testing.T) {
	a, root := testApp(t)
	source := makeProject(t, a, "active", "Matrix")
	writeFile(t, filepath.Join(source, "x.go"), "package x", 0o644)
	if err := saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", GitKind: "none"}}}); err != nil {
		t.Fatal(err)
	}
	a.Mounts = hddOnlyFakeMounts(a.Config)
	if err := os.Remove(a.Config.Stores["archive"].WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"archive", "Matrix", "--yes"}); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("archive without workspace symlink: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("archive changed source before symlink validation")
	}
	external := filepath.Join(root, "external", "ColdImport")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"import", external, "--store", "archive", "--yes", "--exact-root"}); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("archive import without workspace symlink: %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatal("archive import changed source before symlink validation")
	}
}

func TestArchivedRefreshUsesPhysicalBackingWithoutBind(t *testing.T) {
	a, _ := testApp(t)
	physical := makeProject(t, a, "archive", "Cold")
	writeFile(t, filepath.Join(physical, "main.py"), "print('ok')", 0o644)
	if err := saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Cold", Store: "archive", Languages: []string{"java"}, GitKind: "none"}}}); err != nil {
		t.Fatal(err)
	}
	a.Mounts = hddOnlyFakeMounts(a.Config)
	if err := a.Run([]string{"refresh", "Cold"}); err != nil {
		t.Fatal(err)
	}
	reg, _ := a.reloadRegistry()
	if !contains(reg.Projects[0].Languages, "python") || contains(reg.Projects[0].Languages, "java") {
		t.Fatalf("physical archive was not refreshed: %+v", reg.Projects[0])
	}
}

func TestArchivedPathIsLogicalAndRequiresCorrectSymlink(t *testing.T) {
	a, _ := testApp(t)
	physical := makeProject(t, a, "archive", "Cold")
	logical := filepath.Join(a.Config.Stores["archive"].WorkspacePath, "Cold")
	if err := os.MkdirAll(logical, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Cold", Store: "archive", Tags: []string{"old"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"path", "Cold"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(outString(a)); got != logical || got == physical {
		t.Fatalf("path output %q, want logical %q", got, logical)
	}
	resetOutput(a)
	if err := os.Remove(a.Config.Stores["archive"].WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(physical), "wrong"), a.Config.Stores["archive"].WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"path", "Cold"}); err == nil || !strings.Contains(err.Error(), "wrong backing") {
		t.Fatalf("wrong workspace symlink path result: %v", err)
	}
	if outString(a) != "" {
		t.Fatalf("failed path polluted stdout: %q", outString(a))
	}
}

func TestGeneratedArchivePathResolvesOnlyWithHealthyWorkspaceSymlink(t *testing.T) {
	a, _ := testApp(t)
	makeProject(t, a, "archive", "Cold")
	if err := os.MkdirAll(filepath.Join(a.Config.Stores["archive"].WorkspacePath, "Cold"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Cold", Store: "archive", Tags: []string{"old"}, GitKind: "none"}}}
	if _, err := reindexViews(a.Config, reg); err != nil {
		t.Fatal(err)
	}
	view := filepath.Join(a.Config.Views.Tags, "old", "Cold")
	p, _, err := resolveProject(reg, a.Config, a.Mounts, view)
	if err != nil || p.Name != "Cold" {
		t.Fatalf("healthy generated archive path: %+v, %v", p, err)
	}
	if err := os.Remove(a.Config.Stores["archive"].WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveProject(reg, a.Config, a.Mounts, view); err == nil {
		t.Fatal("generated archive path resolved without workspace symlink")
	}
}

func TestRemoveOfflineArchiveRemovesGlobalViews(t *testing.T) {
	a, _ := testApp(t)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Cold", Store: "archive", Tags: []string{"old"}, Languages: []string{"java"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	if _, err := reindexViews(a.Config, reg); err != nil {
		t.Fatal(err)
	}
	a.Mounts = fakeMounts{}
	if err := a.Run([]string{"remove", "Cold", "--yes"}); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{filepath.Join(a.Config.Views.Tags, "old", "Cold"), filepath.Join(a.Config.Views.Languages, "java", "Cold")} {
		if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("offline global view retained: %s", link)
		}
	}
}

func TestArchiveAvailabilityStatesAndDoctor(t *testing.T) {
	a, _ := testApp(t)
	if err := archiveReadyForWrite(a.Config, a.Mounts); err != nil {
		t.Fatal(err)
	}
	a.Mounts = hddOnlyFakeMounts(a.Config, "ro")
	if err := archiveBackingWritable(a.Config, a.Mounts); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only mount accepted: %v", err)
	}
	a.Mounts = hddOnlyFakeMounts(a.Config)
	if err := os.Remove(a.Config.Stores["archive"].WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if err := archiveWorkspaceAvailable(a.Config, a.Mounts); err == nil || !strings.Contains(err.Error(), "symlink missing") {
		t.Fatalf("missing symlink not distinguished: %v", err)
	}
	if err := os.Symlink(filepath.Join(a.Config.Stores["archive"].Mount, "other"), a.Config.Stores["archive"].WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if err := archiveWorkspaceAvailable(a.Config, a.Mounts); err == nil || !strings.Contains(err.Error(), "wrong backing") {
		t.Fatalf("wrong symlink not distinguished: %v", err)
	}
	a.Out = &bytes.Buffer{}
	if err := a.Run([]string{"doctor"}); err == nil {
		t.Fatal("doctor should fail for wrong workspace symlink")
	}
	if !strings.Contains(outString(a), "Archive backing store: AVAILABLE") || !strings.Contains(outString(a), "Archive workspace symlink: MISCONFIGURED") {
		t.Fatal(outString(a))
	}
}

func TestConfigV2AndLegacyRegistryCompatibility(t *testing.T) {
	a, _ := testApp(t)
	text := configText(a.Config, a.Paths.Home)
	if !strings.Contains(text, "version = 2") || !strings.Contains(text, `[workspace]`) || !strings.Contains(text, `workspace_path = `) {
		t.Fatal(text)
	}
	legacy := "version = 1\n\n[stores.active]\nroot = \"~/code\"\n\n[stores.archive]\nroot = \"/mnt/hdd/code\"\nmount = \"/mnt/hdd\"\n"
	if _, err := parseConfig(legacy, a.Paths.Home); err == nil {
		t.Fatal("legacy config silently accepted")
	} else {
		var legacyErr *LegacyConfigError
		if !errors.As(err, &legacyErr) {
			t.Fatalf("legacy config error type: %T %v", err, err)
		}
	}
	registry := "version = 1\n\n[[projects]]\nid = \"stable\"\nname = \"Rune\"\nstore = \"active\"\nlanguages = [\"java\"]\nbuild_systems = []\ntags = []\ngit = false\ngit_kind = \"none\"\n"
	reg, err := parseRegistry(registry)
	if err != nil || len(reg.Projects) != 1 || reg.Projects[0].ID != "stable" {
		t.Fatalf("v1 registry compatibility: %+v, %v", reg, err)
	}
}

func TestPhysicalAndLogicalArchivePathDerivation(t *testing.T) {
	a, _ := testApp(t)
	p := Project{Name: "OldTool", Store: "archive"}
	physical, err := canonicalPath(a.Config, p)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := logicalPath(a.Config, p)
	if err != nil {
		t.Fatal(err)
	}
	if physical != filepath.Join(a.Config.Stores["archive"].Backing, "OldTool") {
		t.Fatalf("physical path %q", physical)
	}
	if logical != filepath.Join(a.Config.Stores["archive"].WorkspacePath, "OldTool") || physical == logical {
		t.Fatalf("logical path %q", logical)
	}
}

func TestImportArchiveUsesNewPhysicalBacking(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "incoming", "OldTool")
	writeFile(t, filepath.Join(source, "main.go"), "package main", 0o644)
	a.RelocationHooks.ForceCopy = true
	if err := a.Run([]string{"import", source, "--store", "archive", "--exact-root", "--tag", "legacy", "--yes"}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(a.Config.Stores["archive"].Backing, "OldTool")
	if _, err := os.Stat(filepath.Join(destination, "main.go")); err != nil {
		t.Fatalf("new physical archive destination missing: %v", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after successful import: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(a.Config.Views.Tags, "legacy", "OldTool")); err != nil || target != "../../archive/OldTool" {
		t.Fatalf("global archive view %q, %v", target, err)
	}
	legacyDestination := filepath.Join(a.Config.Stores["archive"].Mount, "code", "projects", "OldTool")
	if _, err := os.Lstat(legacyDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy archive path was used: %s", legacyDestination)
	}
}

func TestDoctorDistinguishesArchiveFailures(t *testing.T) {
	t.Run("read-only HDD", func(t *testing.T) {
		a, _ := testApp(t)
		mounts := a.Mounts.(fakeMounts)
		mounts.records[0].Options = []string{"ro"}
		a.Mounts = mounts
		if err := a.Run([]string{"doctor"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(outString(a), "HDD backing mount is read-only") {
			t.Fatal(outString(a))
		}
	})
	t.Run("backing missing", func(t *testing.T) {
		a, _ := testApp(t)
		if err := os.Remove(a.Config.Stores["archive"].Backing); err != nil {
			t.Fatal(err)
		}
		if err := a.Run([]string{"doctor"}); err == nil {
			t.Fatal("missing archive backing should be an error")
		}
		if !strings.Contains(outString(a), "archive backing directory missing") {
			t.Fatal(outString(a))
		}
	})
	t.Run("backing not writable", func(t *testing.T) {
		a, _ := testApp(t)
		if err := os.Chmod(a.Config.Stores["archive"].Backing, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(a.Config.Stores["archive"].Backing, 0o755) })
		if err := a.Run([]string{"doctor"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(outString(a), "archive backing directory not writable") {
			t.Fatal(outString(a))
		}
	})
}

func TestArchiveBackingCannotBeOverlaidByWrongFilesystem(t *testing.T) {
	a, _ := testApp(t)
	bad := hddOnlyFakeMounts(a.Config)
	bad.records = append(bad.records, MountRecord{
		Device: "9:9", Root: "/", MountPoint: a.Config.Stores["archive"].Backing,
		FSType: "tmpfs", Options: []string{"rw"},
	})
	if err := archiveBackingAvailable(a.Config, bad); err == nil || !strings.Contains(err.Error(), "expected HDD") {
		t.Fatalf("wrong backing filesystem accepted: %v", err)
	}
}

func TestConfigRejectsOverlappingManagedRootsAndRootMount(t *testing.T) {
	a, _ := testApp(t)
	cfg := a.Config
	cfg.Views.Tags = filepath.Join(cfg.Stores["active"].Backing, "by-tag")
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlapping generated/project roots accepted: %v", err)
	}
	cfg = a.Config
	archive := cfg.Stores["archive"]
	archive.Mount = "/"
	archive.Backing = "/archive"
	cfg.Stores["archive"] = archive
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "cannot be /") {
		t.Fatalf("filesystem root accepted as archive mount: %v", err)
	}
}
