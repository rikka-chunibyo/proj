package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestImportSameFilesystemAndCWDNotice(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "downloads", "Matrix")
	if err := os.MkdirAll(filepath.Join(source, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "go.mod"), "module x", 0o644)
	writeFile(t, filepath.Join(source, "src", "main.go"), "package main", 0o755)
	a.CWD = filepath.Join(source, "src")
	if err := a.Run([]string{"import", ".", "--tag", "Tooling", "--yes"}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(a.Config.Stores["active"].Root, "projects", "Matrix")
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "src", "main.go")); err != nil {
		t.Fatal(err)
	}
	reg, _ := a.reloadRegistry()
	if len(reg.Projects) != 1 || reg.Projects[0].Store != "active" || !contains(reg.Projects[0].Tags, "tooling") {
		t.Fatalf("registry: %+v", reg)
	}
	if !strings.Contains(outString(a), "Your shell was inside") {
		t.Fatal(outString(a))
	}
	journals, _ := filepath.Glob(filepath.Join(a.Paths.StateDir, "transactions", "*.toml"))
	if len(journals) != 0 {
		t.Fatalf("completed journal remains: %v", journals)
	}
}

func TestImportCrossFilesystemCopyPreservesMetadataAndSymlink(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "outside", "Pulse")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, ".hidden"), "secret", 0o600)
	writeFile(t, filepath.Join(source, "run.sh"), "#!/bin/sh\n", 0o755)
	if err := os.Symlink("run.sh", filepath.Join(source, "run-link")); err != nil {
		t.Fatal(err)
	}
	a.RelocationHooks.ForceCopy = true
	if err := a.Run([]string{"import", source, "--yes", "--tag", "Hardware"}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(a.Config.Stores["active"].Root, "projects", "Pulse")
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("source not removed")
	}
	info, err := os.Stat(filepath.Join(destination, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	target, err := os.Readlink(filepath.Join(destination, "run-link"))
	if err != nil || target != "run.sh" {
		t.Fatalf("symlink %q %v", target, err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, ".hidden")); err != nil || string(got) != "secret" {
		t.Fatal("hidden file not preserved")
	}
}

func TestImportSourceChangePreservesSource(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "outside", "Changing")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "a.txt"), "old", 0o644)
	a.RelocationHooks.ForceCopy = true
	a.RelocationHooks.AfterCopy = func(source, temp string) error {
		writeFile(t, filepath.Join(source, "new.txt"), "new", 0o644)
		return nil
	}
	err := a.Run([]string{"import", source, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "source changed materially") {
		t.Fatalf("change result: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "new.txt")); err != nil {
		t.Fatal("source lost")
	}
	destination := filepath.Join(a.Config.Stores["active"].Root, "projects", "Changing")
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("destination masquerades as canonical")
	}
	reg, _ := a.reloadRegistry()
	if len(reg.Projects) != 0 {
		t.Fatal("registry committed on changed source")
	}
}

func TestFreeSpaceAndENOSPCLeaveSourceIntact(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*App)
	}{{"preflight", func(a *App) {
		a.RelocationHooks.ForceCopy = true
		a.RelocationHooks.CheckFreeSpace = func(string, int64) error { return errors.New("not enough free space") }
	}}, {"copy-enospc", func(a *App) {
		a.RelocationHooks.ForceCopy = true
		a.RelocationHooks.CopyTree = func(string, string) error { return syscall.ENOSPC }
	}}} {
		t.Run(tc.name, func(t *testing.T) {
			a, root := testApp(t)
			source := filepath.Join(root, "outside", "Large")
			if err := os.MkdirAll(source, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(source, "data.bin"), strings.Repeat("x", 1024), 0o644)
			tc.configure(a)
			err := a.Run([]string{"import", source, "--yes"})
			if err == nil {
				t.Fatal("failure not reported")
			}
			if _, err := os.Stat(filepath.Join(source, "data.bin")); err != nil {
				t.Fatal("source lost")
			}
			if _, err := os.Stat(filepath.Join(a.Config.Stores["active"].Root, "projects", "Large")); !os.IsNotExist(err) {
				t.Fatal("incomplete destination became canonical")
			}
			reg, _ := a.reloadRegistry()
			if len(reg.Projects) != 0 {
				t.Fatal("registry changed")
			}
		})
	}
}

func TestRegistryCommitFailurePreservesBothCrossFilesystem(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "outside", "Both")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "x.txt"), "x", 0o644)
	a.RelocationHooks.ForceCopy = true
	a.RelocationHooks.BeforeRegistryCommit = func() error { return errors.New("simulated registry failure") }
	err := a.Run([]string{"import", source, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "both preserved") {
		t.Fatalf("commit failure: %v", err)
	}
	destination := filepath.Join(a.Config.Stores["active"].Root, "projects", "Both")
	for _, path := range []string{source, destination} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected preserved %s: %v", path, err)
		}
	}
	reg, _ := a.reloadRegistry()
	if len(reg.Projects) != 0 {
		t.Fatal("failed registry commit changed registry")
	}
	journals, _ := filepath.Glob(filepath.Join(a.Paths.StateDir, "transactions", "*.toml"))
	if len(journals) != 1 {
		t.Fatal("failure journal missing")
	}
}

func TestRegistryCommitFailureRollsBackSameFilesystem(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "outside", "Rollback")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "x.txt"), "x", 0o644)
	a.RelocationHooks.BeforeRegistryCommit = func() error { return errors.New("simulated registry failure") }
	err := a.Run([]string{"import", source, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("source not restored")
	}
	if _, err := os.Stat(filepath.Join(a.Config.Stores["active"].Root, "projects", "Rollback")); !os.IsNotExist(err) {
		t.Fatal("destination retained")
	}
}

func TestCleanupFailurePreservesBothAndJournal(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "outside", "KeepBoth")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "x.go"), "package x", 0o644)
	a.RelocationHooks.ForceCopy = true
	a.RelocationHooks.BeforeCleanup = func(source, destination string) error { return errors.New("simulated cleanup stop") }
	err := a.Run([]string{"import", source, "--yes"})
	if err == nil {
		t.Fatal("cleanup failure hidden")
	}
	destination := filepath.Join(a.Config.Stores["active"].Root, "projects", "KeepBoth")
	for _, p := range []string{source, destination} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("copy %s not preserved: %v", p, err)
		}
	}
	reg, _ := a.reloadRegistry()
	if len(reg.Projects) != 1 {
		t.Fatal("destination not authoritative")
	}
	journals, _ := filepath.Glob(filepath.Join(a.Paths.StateDir, "transactions", "*.toml"))
	if len(journals) != 1 {
		t.Fatalf("journal missing: %v", journals)
	}
	b, _ := os.ReadFile(journals[0])
	if !strings.Contains(string(b), `phase = "cleanup_blocked"`) {
		t.Fatalf("journal: %s", b)
	}
}

func TestSourceIdentityMismatchNeverDeletesReplacement(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	dev, ino, err := sourceIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, source+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "valuable"), "keep", 0o644)
	err = safeRemoveSource(source, dev, ino, a.Paths, a.Config)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("identity check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "valuable")); err != nil {
		t.Fatal("replacement deleted")
	}
}

func TestArchiveRestoreSharedEngine(t *testing.T) {
	a, _ := testApp(t)
	source := makeProject(t, a, "active", "Matrix")
	writeFile(t, filepath.Join(source, "main.rs"), "", 0o644)
	p := Project{ID: "stable", Name: "Matrix", Store: "active", Languages: []string{"rust"}, Tags: []string{"keep"}, GitKind: "none"}
	if err := saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{p}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reindexStore(a.Config, "active", Registry{Version: 1, Projects: []Project{p}}, a.Mounts); err != nil {
		t.Fatal(err)
	}
	a.RelocationHooks.ForceCopy = true
	if err := a.Run([]string{"archive", "matrix", "-y"}); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(a.Config.Stores["archive"].Root, "projects", "Matrix")
	if _, err := os.Stat(archived); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(a.Config.Stores["active"].Root, "by-tag", "keep", "Matrix")); !os.IsNotExist(err) {
		t.Fatal("stale active view remains after archive")
	}
	reg, _ := a.reloadRegistry()
	if reg.Projects[0].ID != "stable" || reg.Projects[0].Store != "archive" || !contains(reg.Projects[0].Tags, "keep") {
		t.Fatalf("archive metadata: %+v", reg.Projects[0])
	}
	if err := a.Run([]string{"archive", "Matrix", "-y"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"restore", "MATRIX", "--yes"}); err != nil {
		t.Fatal(err)
	}
	reg, _ = a.reloadRegistry()
	if reg.Projects[0].Store != "active" || reg.Projects[0].ID != "stable" {
		t.Fatalf("restore: %+v", reg.Projects[0])
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"restore", "Matrix", "--yes"}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveMountAndDestinationCollision(t *testing.T) {
	a, _ := testApp(t)
	source := makeProject(t, a, "active", "Matrix")
	_ = source
	p := Project{ID: "1", Name: "Matrix", Store: "active", GitKind: "none"}
	_ = saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{p}})
	mount := a.Config.Stores["archive"].Mount
	a.Mounts = fakeMounts{}
	err := a.Run([]string{"archive", "Matrix", "-y"})
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("mount: %v", err)
	}
	if _, err := os.Stat(a.Config.Stores["archive"].Root); !os.IsNotExist(err) {
		t.Fatal("archive root created while unmounted")
	}
	a.Mounts = fakeMounts{mount: true}
	makeProject(t, a, "archive", "Matrix")
	err = a.Run([]string{"archive", "Matrix", "-y"})
	if err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("collision: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("source lost")
	}
}

func TestArchiveRootSymlinkIsRefused(t *testing.T) {
	a, root := testApp(t)
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, a.Config.Stores["archive"].Root); err != nil {
		t.Fatal(err)
	}
	source := makeProject(t, a, "active", "Matrix")
	_ = source
	_ = saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", GitKind: "none"}}})
	err := a.Run([]string{"archive", "Matrix", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("archive symlink root: %v", err)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote through archive root symlink")
	}
}

func TestRelocationRefusesGitWorktreeStates(t *testing.T) {
	for _, kind := range []string{"git-file/worktree-style", "main-repo-with-linked-worktrees"} {
		t.Run(kind, func(t *testing.T) {
			a, root := testApp(t)
			source := filepath.Join(root, "outside", "GitProj")
			if err := os.MkdirAll(source, 0o755); err != nil {
				t.Fatal(err)
			}
			if kind == "git-file/worktree-style" {
				writeFile(t, filepath.Join(source, ".git"), "gitdir: /x", 0o644)
			} else {
				if err := os.MkdirAll(filepath.Join(source, ".git", "worktrees", "x"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err := a.Run([]string{"import", source, "--yes"})
			if err == nil || !strings.Contains(err.Error(), "relocation refused") {
				t.Fatalf("result: %v", err)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatal("source touched")
			}
		})
	}
}

func TestRelocationRefusesUnsupportedFIFO(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "outside", "PipeProj")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	err := a.Run([]string{"import", source, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "unsupported filesystem object") {
		t.Fatalf("FIFO result: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("source removed")
	}
}

func TestImportRootDetectionAndAmbiguity(t *testing.T) {
	a, root := testApp(t)
	project := filepath.Join(root, "outer", "Project")
	nested := filepath.Join(project, "src", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, "Cargo.toml"), "", 0o644)
	if err := a.Run([]string{"import", nested, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Config.Stores["active"].Root, "projects", "Project")); err != nil {
		t.Fatal(err)
	}
	a2, root2 := testApp(t)
	parent := filepath.Join(root2, "mono")
	child := filepath.Join(parent, "child")
	deep := filepath.Join(child, "src")
	_ = os.MkdirAll(deep, 0o755)
	writeFile(t, filepath.Join(parent, ".git", "HEAD"), "", 0o644)
	writeFile(t, filepath.Join(child, "package.json"), "{}", 0o644)
	err := a2.Run([]string{"import", deep, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "multiple project roots") {
		t.Fatalf("ambiguity: %v", err)
	}
	a3, root3 := testApp(t)
	plain := filepath.Join(root3, "Plain")
	_ = os.MkdirAll(plain, 0o755)
	if err := a3.Run([]string{"import", plain, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a3.Config.Stores["active"].Root, "projects", "Plain")); err != nil {
		t.Fatal(err)
	}
}

func TestImportExactRootBypassesAncestorMarker(t *testing.T) {
	a, root := testApp(t)
	parent := filepath.Join(root, "repo")
	source := filepath.Join(parent, "tools", "small-script")
	if err := os.MkdirAll(filepath.Join(parent, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "script.sh"), "#!/bin/sh\n", 0o755)
	if err := a.Run([]string{"import", source, "--exact-root", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Config.Stores["active"].Root, "projects", "small-script", "script.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatal("ancestor repository was moved")
	}
}

func TestImportTopSymlinkAndWindowsNameRefusal(t *testing.T) {
	a, root := testApp(t)
	real := filepath.Join(root, "real")
	_ = os.Mkdir(real, 0o755)
	link := filepath.Join(root, "link")
	_ = os.Symlink(real, link)
	if err := a.Run([]string{"import", link, "--yes"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink: %v", err)
	}
	bad := filepath.Join(root, "CON")
	_ = os.Mkdir(bad, 0o755)
	if err := a.Run([]string{"import", bad, "--yes"}); err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Fatalf("Windows name: %v", err)
	}
}

func TestExternalSymlinkWarning(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(root, "LinkProj")
	_ = os.Mkdir(source, 0o755)
	_ = os.Symlink("../external", filepath.Join(source, "outside"))
	if err := a.Run([]string{"import", source, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errString(a), "symlink escapes project root") {
		t.Fatal(errString(a))
	}
	target, _ := os.Readlink(filepath.Join(a.Config.Stores["active"].Root, "projects", "LinkProj", "outside"))
	if target != "../external" {
		t.Fatal("symlink dereferenced or changed")
	}
}
