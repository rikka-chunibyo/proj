package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRefreshTagsPathInfoAndRemove(t *testing.T) {
	a, _ := testApp(t)
	root := makeProject(t, a, "active", "Matrix")
	writeFile(t, filepath.Join(root, "src", "Main.java"), "class Main {}", 0o644)
	writeFile(t, filepath.Join(root, "build.gradle"), "", 0o644)
	a.CWD = filepath.Join(root, "src")
	if err := a.Run([]string{"add", ".", "--tag", "Minecraft", "--tag", "Client"}); err != nil {
		t.Fatal(err)
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Projects) != 1 {
		t.Fatal("not registered")
	}
	p := reg.Projects[0]
	if p.Name != "Matrix" || strings.Join(p.Tags, ",") != "client,minecraft" || !contains(p.Languages, "java") || !contains(p.BuildSystems, "gradle") {
		t.Fatalf("project: %+v", p)
	}
	if _, err := os.Lstat(filepath.Join(a.Config.Stores["active"].Root, "by-tag", "minecraft", "Matrix")); err != nil {
		t.Fatal(err)
	}
	resetOutput(a)
	if err := a.Run([]string{"add", root, "--tag", "ignored"}); err != nil {
		t.Fatal(err)
	}
	reg, _ = a.reloadRegistry()
	if contains(reg.Projects[0].Tags, "ignored") {
		t.Fatal("re-add changed metadata")
	}
	if !strings.Contains(outString(a), "already registered") {
		t.Fatal(outString(a))
	}
	resetOutput(a)
	if err := a.Run([]string{"tag", "add", "MATRIX", "Logic Analyzer", "client"}); err != nil {
		t.Fatal(err)
	}
	reg, _ = a.reloadRegistry()
	if strings.Join(reg.Projects[0].Tags, ",") != "client,logic-analyzer,minecraft" {
		t.Fatalf("tags: %v", reg.Projects[0].Tags)
	}
	if err := a.Run([]string{"tag", "remove", "matrix", "missing", "client"}); err != nil {
		t.Fatal(err)
	}
	reg, _ = a.reloadRegistry()
	if strings.Join(reg.Projects[0].Tags, ",") != "logic-analyzer,minecraft" {
		t.Fatalf("tags: %v", reg.Projects[0].Tags)
	}
	writeFile(t, filepath.Join(root, "tool.py"), "print('x')", 0o644)
	if err := a.Run([]string{"refresh", "mAtRiX"}); err != nil {
		t.Fatal(err)
	}
	reg, _ = a.reloadRegistry()
	if !contains(reg.Projects[0].Languages, "python") {
		t.Fatal("refresh did not detect python")
	}
	resetOutput(a)
	if err := a.Run([]string{"path", "matrix"}); err != nil {
		t.Fatal(err)
	}
	if outString(a) != root+"\n" {
		t.Fatalf("path stdout not clean: %q", outString(a))
	}
	resetOutput(a)
	if err := a.Run([]string{"info", "MATRIX"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outString(a), "Status:\n  available") || !strings.Contains(outString(a), "Python") {
		t.Fatal(outString(a))
	}
	resetOutput(a)
	if err := a.Run([]string{"remove", "matrix", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("remove touched source")
	}
	reg, _ = a.reloadRegistry()
	if len(reg.Projects) != 0 {
		t.Fatal("entry retained")
	}
}

func TestAddOutsideAndArchiveUnavailable(t *testing.T) {
	a, root := testApp(t)
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	err := a.Run([]string{"add", outside})
	if err == nil || !strings.Contains(err.Error(), "proj import") {
		t.Fatalf("outside add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Config.Stores["active"].Root, "projects", "outside")); !os.IsNotExist(err) {
		t.Fatal("outside add moved data")
	}
	arch := makeProject(t, a, "archive", "Cold")
	a.Mounts = fakeMounts{}
	err = a.Run([]string{"add", arch})
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("offline archive add: %v", err)
	}
	reg, _ := a.reloadRegistry()
	if len(reg.Projects) != 0 {
		t.Fatal("offline project registered")
	}
}

func TestAddGeneratedViewAndCanonicalSymlinkRefusal(t *testing.T) {
	a, _ := testApp(t)
	root := makeProject(t, a, "active", "Matrix")
	writeFile(t, filepath.Join(root, "x.go"), "package x", 0o644)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", Tags: []string{"go-project"}, Languages: []string{"go"}, GitKind: "none"}}}
	if _, err := reindexStore(a.Config, "active", reg, a.Mounts); err != nil {
		t.Fatal(err)
	}
	if err := saveRegistry(a.Paths.RegistryFile, emptyRegistry()); err != nil {
		t.Fatal(err)
	}
	view := filepath.Join(a.Config.Stores["active"].Root, "by-tag", "go-project", "Matrix")
	if err := a.Run([]string{"add", view}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.reloadRegistry()
	if got.Projects[0].Name != "Matrix" {
		t.Fatal("view did not resolve canonical")
	}
	real := filepath.Join(t.TempDir(), "real")
	_ = os.Mkdir(real, 0o755)
	link := filepath.Join(a.Config.Stores["active"].Root, "projects", "Link")
	_ = os.Symlink(real, link)
	if err := a.Run([]string{"add", link}); err == nil {
		t.Fatal("canonical symlink accepted")
	}
}

func TestFindListDeterminismAndFilters(t *testing.T) {
	a, _ := testApp(t)
	reg := Registry{Version: 1, Projects: []Project{{ID: "3", Name: "Zebra", Store: "active", Languages: []string{"java"}, Tags: []string{"minecraft"}, BuildSystems: []string{"gradle"}, GitKind: "none"}, {ID: "1", Name: "PulseMonitor", Store: "active", Languages: []string{"cpp", "python"}, Tags: []string{"hardware", "tooling"}, GitKind: "none"}, {ID: "2", Name: "Alpha", Store: "archive", Languages: []string{"java"}, Tags: []string{"minecraft"}, BuildSystems: []string{"maven"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"list"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(outString(a)), "\n")
	if !strings.HasPrefix(lines[0], "Alpha") || !strings.HasPrefix(lines[1], "PulseMonitor") || !strings.HasPrefix(lines[2], "Zebra") {
		t.Fatalf("order: %v", lines)
	}
	resetOutput(a)
	if err := a.Run([]string{"find", "pulsmon"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outString(a), "PulseMonitor") {
		t.Fatal("fuzzy typo not found")
	}
	resetOutput(a)
	if err := a.Run([]string{"find", "java", "minecraft"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outString(a), "Alpha") || !strings.Contains(outString(a), "Zebra") {
		t.Fatal(outString(a))
	}
	resetOutput(a)
	if err := a.Run([]string{"find", "java", "hardware"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(outString(a)) != "" {
		t.Fatalf("AND semantics failed: %q", outString(a))
	}
	resetOutput(a)
	if err := a.Run([]string{"list", "--archived", "--build", "maven"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outString(a), "Alpha") || strings.Contains(outString(a), "Zebra") {
		t.Fatal(outString(a))
	}
}

func TestPathFailsOfflineAndInfoStillWorks(t *testing.T) {
	a, _ := testApp(t)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Cold", Store: "archive", Tags: []string{"old"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	a.Mounts = fakeMounts{}
	if err := a.Run([]string{"path", "Cold"}); err == nil {
		t.Fatal("offline path succeeded")
	}
	resetOutput(a)
	if err := a.Run([]string{"info", "cold"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outString(a), "unavailable/unverified") {
		t.Fatal(outString(a))
	}
	resetOutput(a)
	if err := a.Run([]string{"list", "--archived"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outString(a), "Cold") {
		t.Fatal("offline archive hidden")
	}
}

func TestRefreshAllSkipsOfflineArchive(t *testing.T) {
	a, _ := testApp(t)
	active := makeProject(t, a, "active", "Live")
	writeFile(t, filepath.Join(active, "x.rs"), "", 0o644)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Live", Store: "active", GitKind: "none"}, {ID: "2", Name: "Cold", Store: "archive", Languages: []string{"java"}, GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	a.Mounts = fakeMounts{}
	if err := a.Run([]string{"refresh", "--all"}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.reloadRegistry()
	if !contains(got.Projects[1].Languages, "rust") && got.Projects[1].Name == "Live" {
		t.Fatal("active not refreshed")
	}
	for _, p := range got.Projects {
		if p.Name == "Cold" && !contains(p.Languages, "java") {
			t.Fatal("offline metadata erased")
		}
	}
}

func TestRepairPreservesMetadataAndRejectsCollision(t *testing.T) {
	a, _ := testApp(t)
	old := Project{ID: "stable", Name: "Old", Store: "active", Tags: []string{"keep"}, Languages: []string{"go"}, BuildSystems: []string{"go-modules"}, GitKind: "none"}
	reg := Registry{Version: 1, Projects: []Project{old, {ID: "other", Name: "Taken", Store: "active", GitKind: "none"}}}
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		t.Fatal(err)
	}
	newPath := makeProject(t, a, "archive", "Renamed")
	if err := a.Run([]string{"repair", "Old", newPath}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.reloadRegistry()
	var found Project
	for _, p := range got.Projects {
		if p.ID == "stable" {
			found = p
		}
	}
	if found.Name != "Renamed" || found.Store != "archive" || !contains(found.Tags, "keep") || !contains(found.Languages, "go") {
		t.Fatalf("repair: %+v", found)
	}
	takenPath := filepath.Join(a.Config.Stores["active"].Root, "projects", "taken")
	if err := os.MkdirAll(takenPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.Run([]string{"repair", "Renamed", takenPath}); err == nil {
		t.Fatal("collision repair accepted")
	}
	outside := t.TempDir()
	if err := a.Run([]string{"repair", "Renamed", outside}); err == nil {
		t.Fatal("outside repair accepted")
	}
}

func TestTagOfflineMutationAndAliases(t *testing.T) {
	a, _ := testApp(t)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Cold", Store: "archive", GitKind: "none"}}}
	_ = saveRegistry(a.Paths.RegistryFile, reg)
	a.Mounts = fakeMounts{}
	if err := a.Run([]string{"tag", "add", "Cold", "Research"}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.reloadRegistry()
	if !contains(got.Projects[0].Tags, "research") {
		t.Fatal("offline tag not saved")
	}
	for _, alias := range []string{"forget", "unregister"} {
		a2, _ := testApp(t)
		path := makeProject(t, a2, "active", "Keep")
		_ = saveRegistry(a2.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Keep", Store: "active", GitKind: "none"}}})
		if err := a2.Run([]string{alias, "Keep", "-y"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s deleted data", alias)
		}
	}
}

func TestNoninteractiveConfirmation(t *testing.T) {
	a, _ := testApp(t)
	p := makeProject(t, a, "active", "Matrix")
	_ = saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", GitKind: "none"}}})
	err := a.Run([]string{"remove", "Matrix"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("confirmation: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("source changed")
	}
}

func TestHelpAndVersionNoStorageMutation(t *testing.T) {
	a, _ := testApp(t)
	for _, args := range [][]string{{"--help"}, {"--version"}, {"list"}, {"find", "nothing"}} {
		resetOutput(a)
		if err := a.Run(args); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(a.Config.Stores["active"].Root); err != nil {
		t.Fatal("preexisting temp active root vanished")
	}
	if _, err := os.Stat(a.Paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatal("read-only command created config")
	}
	if _, err := os.Stat(a.Paths.RegistryFile); !os.IsNotExist(err) {
		t.Fatal("read-only command created registry")
	}
	if bytes.Contains(a.Out.(*bytes.Buffer).Bytes(), []byte("\x1b")) {
		t.Fatal("unexpected ANSI")
	}
}

func TestConcurrentTagMutationsAreSerialized(t *testing.T) {
	a, _ := testApp(t)
	makeProject(t, a, "active", "Matrix")
	if err := saveRegistry(a.Paths.RegistryFile, Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", GitKind: "none"}}}); err != nil {
		t.Fatal(err)
	}
	clone := func() *App {
		return &App{Paths: a.Paths, Config: a.Config, Mounts: a.Mounts, In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, CWD: a.CWD}
	}
	a1, a2 := clone(), clone()
	done := make(chan error, 2)
	go func() { done <- a1.Run([]string{"tag", "add", "Matrix", "one"}) }()
	go func() { done <- a2.Run([]string{"tag", "add", "Matrix", "two"}) }()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(reg.Projects[0].Tags, "one") || !contains(reg.Projects[0].Tags, "two") {
		t.Fatalf("lost concurrent update: %v", reg.Projects[0].Tags)
	}
}
