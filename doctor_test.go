package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorOfflineArchiveIsNotMassMissing(t *testing.T) {
	a, _ := testApp(t)
	live := makeProject(t, a, "active", "Live")
	_ = live
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Live", Store: "active", GitKind: "none"}, {ID: "2", Name: "ColdOne", Store: "archive", GitKind: "none"}, {ID: "3", Name: "ColdTwo", Store: "archive", GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	a.Mounts = fakeMounts{}
	if err := a.Run([]string{"doctor"}); err != nil {
		t.Fatal(err)
	}
	out := outString(a)
	if !strings.Contains(out, "HDD backing mount unavailable") || !strings.Contains(out, "2 archived project(s)") {
		t.Fatal(out)
	}
	if strings.Contains(out, "ColdOne: canonical project missing") || strings.Contains(out, "ColdTwo: canonical project missing") {
		t.Fatal("offline archive reported projects missing")
	}
	if !strings.Contains(out, "No changes were made") {
		t.Fatal(out)
	}
}

func TestDoctorFindsMissingUnregisteredViewsAndTransaction(t *testing.T) {
	a, _ := testApp(t)
	if err := ensureStore(a.Config, "active", a.Mounts); err != nil {
		t.Fatal(err)
	}
	makeProject(t, a, "active", "Unregistered")
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Missing", Store: "active", Tags: []string{"tag"}, Languages: []string{"go"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	txn := Transaction{ID: "txn-test", ProjectID: "1", Operation: "archive", Source: "/source", Destination: "/destination", Phase: "destination_finalized", Started: time.Now().UTC().Format(time.RFC3339)}
	if err := saveTransaction(a.Paths.StateDir, txn); err != nil {
		t.Fatal(err)
	}
	err := a.Run([]string{"doctor"})
	if err == nil {
		t.Fatal("doctor should fail on missing active project")
	}
	out := outString(a)
	for _, want := range []string{"canonical backing project missing", "unregistered canonical directory", "incomplete relocation transaction txn-test", "No changes were made"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(a.Config.Stores["active"].Backing, "Unregistered")); err != nil {
		t.Fatal("doctor mutated data")
	}
	if _, err := os.Stat(filepath.Join(a.Paths.StateDir, "transactions", "txn-test.toml")); err != nil {
		t.Fatal("doctor removed journal")
	}
}

func TestDoctorReportsBadGeneratedObjectsAndEscapes(t *testing.T) {
	a, _ := testApp(t)
	makeProject(t, a, "active", "Matrix")
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", Tags: []string{"safe"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	root := a.Config.Views.Tags
	if err := os.MkdirAll(filepath.Join(root, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "safe", "Matrix"), "user data", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "escape"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../outside", filepath.Join(root, "escape", "Bad")); err != nil {
		t.Fatal(err)
	}
	err := a.Run([]string{"doctor"})
	if err == nil {
		t.Fatal("escaping view should be error")
	}
	out := outString(a)
	if !strings.Contains(out, "generated link escapes") || !strings.Contains(out, "unexpected regular file or directory") {
		t.Fatal(out)
	}
	b, err := os.ReadFile(filepath.Join(root, "safe", "Matrix"))
	if err != nil || string(b) != "user data" {
		t.Fatal("doctor modified unexpected file")
	}
}

func TestTransactionLifecycle(t *testing.T) {
	a, _ := testApp(t)
	txn := Transaction{ID: "abc", Operation: "import", Source: "/a", Destination: "/b", Phase: "copying", Started: "now", SourceDevice: 1, SourceInode: 2}
	if err := saveTransaction(a.Paths.StateDir, txn); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Paths.StateDir, "transactions", "abc.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`operation = "import"`, `phase = "copying"`, `source_device = 1`, `source_inode = 2`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("journal missing %q:\n%s", want, b)
		}
	}
	if err := finishTransaction(a.Paths.StateDir, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("completed journal remains")
	}
}

func TestDoctorWarnsWindowsNameAndGitWorktree(t *testing.T) {
	a, _ := testApp(t)
	root := makeProject(t, a, "active", "CON")
	writeFile(t, filepath.Join(root, ".git"), "gitdir: /elsewhere", 0o644)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "CON", Store: "active", Git: true, GitKind: "git-file/worktree-style"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"doctor"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outString(a), "Windows/Samba-dangerous") || !strings.Contains(outString(a), "Git relocation-sensitive") {
		t.Fatal(outString(a))
	}
}
