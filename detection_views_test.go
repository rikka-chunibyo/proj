package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLanguageAndBuildDetection(t *testing.T) {
	cases := []struct{ name, file, lang, build string }{
		{"java", "src/Main.java", "java", "maven"}, {"kotlin", "src/Main.kt", "kotlin", "gradle"}, {"c", "src/main.c", "c", "cmake"}, {"cpp", "src/main.cpp", "cpp", "cmake"}, {"csharp", "App.cs", "csharp", "msbuild"}, {"go", "main.go", "go", "go-modules"}, {"rust", "src/lib.rs", "rust", "cargo"}, {"python", "app.py", "python", ""}, {"javascript", "src/app.js", "javascript", "npm"}, {"typescript", "src/app.ts", "typescript", "npm"}, {"lua", "main.lua", "lua", ""}, {"shell", "run.sh", "shell", ""}, {"html", "index.html", "html", ""}, {"css", "style.css", "css", ""}, {"sql", "schema.sql", "sql", ""}, {"assembly", "boot.asm", "assembly", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, tc.file), "source", 0o644)
			switch tc.build {
			case "maven":
				writeFile(t, filepath.Join(root, "pom.xml"), "", 0o644)
			case "gradle":
				writeFile(t, filepath.Join(root, "build.gradle.kts"), "", 0o644)
			case "cmake":
				writeFile(t, filepath.Join(root, "CMakeLists.txt"), "", 0o644)
			case "msbuild":
				writeFile(t, filepath.Join(root, "App.csproj"), "", 0o644)
			case "go-modules":
				writeFile(t, filepath.Join(root, "go.mod"), "", 0o644)
			case "cargo":
				writeFile(t, filepath.Join(root, "Cargo.toml"), "", 0o644)
			case "npm":
				writeFile(t, filepath.Join(root, "package.json"), "{}", 0o644)
			}
			d, err := detectProject(root)
			if err != nil {
				t.Fatal(err)
			}
			if !contains(d.Languages, tc.lang) {
				t.Fatalf("language %s not detected: %+v", tc.lang, d)
			}
			if tc.build != "" && !contains(d.BuildSystems, tc.build) {
				t.Fatalf("build %s not detected: %+v", tc.build, d)
			}
		})
	}
}

func TestDetectionIgnoresDataGeneratedAndSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"data.json", "x.yaml", "x.toml", "x.xml", "README.md", "build.gradle.kts"} {
		writeFile(t, filepath.Join(root, name), "x", 0o644)
	}
	writeFile(t, filepath.Join(root, "node_modules", "bad.go"), "x", 0o644)
	writeFile(t, filepath.Join(root, "target", "bad.rs"), "x", 0o644)
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	d, err := detectProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Languages) != 0 {
		t.Fatalf("non-substantive language detected: %+v", d.Languages)
	}
	if !contains(d.BuildSystems, "gradle") {
		t.Fatal("Gradle not detected")
	}
}

func TestMultiLanguageAndGitKinds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.cpp"), "", 0o644)
	writeFile(t, filepath.Join(root, "tools", "x.py"), "", 0o644)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := detectProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.Languages, ",") != "cpp,python" || d.GitKind != "directory" {
		t.Fatalf("unexpected detection: %+v", d)
	}
	root2 := t.TempDir()
	writeFile(t, filepath.Join(root2, ".git"), "gitdir: /tmp/x", 0o644)
	d, err = detectProject(root2)
	if err != nil || d.GitKind != "git-file/worktree-style" {
		t.Fatalf("git file: %+v %v", d, err)
	}
	root3 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root3, ".git", "worktrees", "linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err = detectProject(root3)
	if err != nil || d.GitKind != "main-repo-with-linked-worktrees" {
		t.Fatalf("linked worktrees: %+v %v", d, err)
	}
}

func TestViewsReconcileSafely(t *testing.T) {
	a, _ := testApp(t)
	matrix := makeProject(t, a, "active", "Matrix")
	_ = matrix
	pulse := makeProject(t, a, "active", "Pulse")
	_ = pulse
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", Tags: []string{"minecraft", "client"}, Languages: []string{"java"}, GitKind: "none"}, {ID: "2", Name: "Pulse", Store: "active", Tags: []string{"hardware", "tooling"}, Languages: []string{"cpp", "python"}, GitKind: "none"}}}
	report, err := reindexStore(a.Config, "active", reg, a.Mounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 || len(report.Missing) != 0 {
		t.Fatalf("report %+v", report)
	}
	for _, spec := range []struct{ kind, cat, name string }{{"by-tag", "minecraft", "Matrix"}, {"by-tag", "hardware", "Pulse"}, {"by-language", "java", "Matrix"}, {"by-language", "cpp", "Pulse"}, {"by-language", "python", "Pulse"}} {
		path := filepath.Join(a.Config.Stores["active"].Root, spec.kind, spec.cat, spec.name)
		target, err := os.Readlink(path)
		if err != nil || target != "../../projects/"+spec.name {
			t.Fatalf("link %s -> %q, %v", path, target, err)
		}
	}
	staleDir := filepath.Join(a.Config.Stores["active"].Root, "by-tag", "stale")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../projects/Nope", filepath.Join(staleDir, "Nope")); err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(a.Config.Stores["active"].Root, "by-tag", "minecraft", "Matrix")
	if err := os.Remove(wrong); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../projects/Pulse", wrong); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(a.Config.Stores["active"].Root, "by-tag", "minecraft", "notes.txt")
	writeFile(t, protected, "keep", 0o644)
	report, err = reindexStore(a.Config, "active", reg, a.Mounts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(staleDir, "Nope")); !os.IsNotExist(err) {
		t.Fatal("stale symlink retained")
	}
	target, _ := os.Readlink(wrong)
	if target != "../../projects/Matrix" {
		t.Fatalf("wrong link not fixed: %q", target)
	}
	if b, err := os.ReadFile(protected); err != nil || string(b) != "keep" {
		t.Fatal("unexpected file deleted")
	}
	if len(report.Warnings) == 0 {
		t.Fatal("unexpected regular file not reported")
	}
}

func TestViewsSkipMissingAndFullRebuild(t *testing.T) {
	a, _ := testApp(t)
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Missing", Store: "active", Tags: []string{"x"}, Languages: []string{"go"}, GitKind: "none"}}}
	report, err := reindexStore(a.Config, "active", reg, a.Mounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing not reported: %+v", report)
	}
	if _, err := os.Lstat(filepath.Join(a.Config.Stores["active"].Root, "by-tag", "x", "Missing")); !os.IsNotExist(err) {
		t.Fatal("broken link created")
	}
	makeProject(t, a, "active", "Missing")
	report, err = reindexStore(a.Config, "active", reg, a.Mounts)
	if err != nil || len(report.Missing) != 0 {
		t.Fatalf("rebuild: %+v %v", report, err)
	}
	if _, err := os.Lstat(filepath.Join(a.Config.Stores["active"].Root, "by-language", "go", "Missing")); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedRootSymlinkIsRefused(t *testing.T) {
	a, root := testApp(t)
	makeProject(t, a, "active", "Matrix")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	viewRoot := filepath.Join(a.Config.Stores["active"].Root, "by-tag")
	if err := os.Symlink(outside, viewRoot); err != nil {
		t.Fatal(err)
	}
	reg := Registry{Version: 1, Projects: []Project{{ID: "1", Name: "Matrix", Store: "active", Tags: []string{"tag"}, GitKind: "none"}}}
	if _, err := reindexStore(a.Config, "active", reg, a.Mounts); err == nil {
		t.Fatal("generated-root symlink accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("reindex escaped generated root")
	}
}
