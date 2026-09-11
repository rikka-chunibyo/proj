package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Detection struct {
	Languages    []string
	BuildSystems []string
	Git          bool
	GitKind      string
}

var ignoredScanDirs = map[string]bool{
	".git": true, ".idea": true, ".vs": true, ".gradle": true,
	".tools":       true,
	"node_modules": true, "build": true, "out": true, "target": true,
	"dist": true, ".venv": true, "venv": true, "vendor": true,
	"__pycache__": true,
}

var languageExtensions = map[string]string{
	".java": "java", ".kt": "kotlin", ".kts": "kotlin",
	".c": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp", ".hxx": "cpp",
	".cs": "csharp", ".go": "go", ".rs": "rust", ".py": "python",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".mts": "typescript", ".cts": "typescript",
	".lua": "lua", ".sh": "shell", ".bash": "shell", ".zsh": "shell", ".fish": "shell",
	".html": "html", ".htm": "html", ".css": "css", ".scss": "css", ".sass": "css", ".less": "css",
	".sql": "sql", ".s": "assembly", ".asm": "assembly", ".nasm": "assembly",
}

func detectProject(root string) (Detection, error) {
	if _, err := validateRealDirectory(root); err != nil {
		return Detection{}, err
	}
	languages := map[string]bool{}
	builds := map[string]bool{}
	hasCHeader := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() && ignoredScanDirs[name] {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil
		}
		lower := strings.ToLower(name)
		switch lower {
		case "pom.xml":
			builds["maven"] = true
		case "settings.gradle", "settings.gradle.kts", "build.gradle", "build.gradle.kts":
			builds["gradle"] = true
		case "cargo.toml":
			builds["cargo"] = true
		case "go.mod":
			builds["go-modules"] = true
		case "package.json":
			builds["npm"] = true
		case "pnpm-lock.yaml", "pnpm-workspace.yaml":
			builds["pnpm"] = true
		case "yarn.lock":
			builds["yarn"] = true
		case "cmakelists.txt":
			builds["cmake"] = true
		}
		if strings.HasSuffix(lower, ".sln") || strings.HasSuffix(lower, ".csproj") || strings.HasSuffix(lower, ".vcxproj") {
			builds["msbuild"] = true
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext == ".h" {
			hasCHeader = true
		}
		if lang, ok := languageExtensions[ext]; ok {
			// Gradle Kotlin DSL is build configuration, not substantive Kotlin source.
			if lang == "kotlin" && (lower == "build.gradle.kts" || lower == "settings.gradle.kts") {
				return nil
			}
			languages[lang] = true
		}
		return nil
	})
	if err != nil {
		return Detection{}, fmt.Errorf("scan project: %w", err)
	}
	if hasCHeader && !languages["cpp"] {
		languages["c"] = true
	}
	git, kind, err := detectGit(root)
	if err != nil {
		return Detection{}, err
	}
	return Detection{Languages: keys(languages), BuildSystems: keys(builds), Git: git, GitKind: kind}, nil
}

func detectGit(root string) (bool, string, error) {
	path := filepath.Join(root, ".git")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "none", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("inspect .git: %w", err)
	}
	if info.Mode().IsRegular() {
		return true, "git-file/worktree-style", nil
	}
	if !info.IsDir() {
		return true, "other", nil
	}
	entries, err := os.ReadDir(filepath.Join(path, "worktrees"))
	if err == nil && len(entries) > 0 {
		return true, "main-repo-with-linked-worktrees", nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "", fmt.Errorf("inspect linked worktrees: %w", err)
	}
	return true, "directory", nil
}

func keys(m map[string]bool) []string {
	result := make([]string, 0, len(m))
	for key := range m {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func findByNameOrID(reg Registry, value string) (*Project, int, error) {
	for i := range reg.Projects {
		if reg.Projects[i].ID == value {
			return &reg.Projects[i], i, nil
		}
	}
	for i := range reg.Projects {
		if strings.EqualFold(reg.Projects[i].Name, value) {
			return &reg.Projects[i], i, nil
		}
	}
	return nil, -1, fmt.Errorf("project %q is not registered", value)
}

func resolveProject(reg Registry, cfg Config, mounts MountProvider, value string) (*Project, int, error) {
	if p, i, err := findByNameOrID(reg, value); err == nil {
		return p, i, nil
	}
	if value != "." && !filepath.IsAbs(value) && !strings.ContainsRune(value, filepath.Separator) {
		return nil, -1, fmt.Errorf("project %q is not registered (direct resolution does not use fuzzy matching; try proj find %s)", value, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return nil, -1, err
	}
	canonical, store, err := managedProjectForPath(cfg, mounts, abs, true)
	if err != nil {
		return nil, -1, fmt.Errorf("resolve path %s: %w", abs, err)
	}
	name := filepath.Base(canonical)
	for i := range reg.Projects {
		if reg.Projects[i].Store == store && strings.EqualFold(reg.Projects[i].Name, name) {
			return &reg.Projects[i], i, nil
		}
	}
	return nil, -1, fmt.Errorf("path %s is not inside a registered canonical project", abs)
}

// canonicalDirectoryForPath returns the direct child of a configured projects
// root that contains path. Generated proj views work because EvalSymlinks is
// applied first. Canonical child symlinks are still rejected by Lstat below.
func canonicalDirectoryForPath(cfg Config, mounts MountProvider, value string) (string, string, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", "", err
	}
	return managedProjectForPath(cfg, mounts, abs, true)
}

var errOutsideManaged = errors.New("path is outside configured canonical project stores")

// managedProjectForPath maps physical active/archive paths and the logical
// archive access symlink to the single physical canonical directory. Symlinks are
// resolved only beneath proj's generated roots; arbitrary external symlinks do
// not acquire project identity merely because they point at a project.
func managedProjectForPath(cfg Config, mounts MountProvider, abs string, allowViews bool) (string, string, error) {
	path := filepath.Clean(abs)
	if allowViews && (pathWithin(path, cfg.Views.Tags) || pathWithin(path, cfg.Views.Languages)) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", "", err
		}
		path = resolved
	}
	for _, candidate := range []struct {
		root, backing, store string
		logical              bool
	}{
		{root: cfg.Stores["active"].Backing, backing: cfg.Stores["active"].Backing, store: "active"},
		{root: cfg.Stores["archive"].Backing, backing: cfg.Stores["archive"].Backing, store: "archive"},
		{root: cfg.Stores["archive"].WorkspacePath, backing: cfg.Stores["archive"].Backing, store: "archive", logical: true},
	} {
		rel, err := filepath.Rel(candidate.root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if candidate.logical {
			if err := archiveWorkspaceAvailable(cfg, mounts); err != nil {
				return "", "", fmt.Errorf("archive workspace path is unavailable or misconfigured: %w", err)
			}
		}
		name := strings.Split(rel, string(filepath.Separator))[0]
		canonical := filepath.Join(candidate.backing, name)
		if _, err := validateRealDirectory(canonical); err != nil {
			return "", "", err
		}
		return canonical, candidate.store, nil
	}
	return path, "", errOutsideManaged
}

var strongRootMarkers = map[string]bool{
	".git": true, "go.mod": true, "cargo.toml": true, "pom.xml": true,
	"settings.gradle": true, "settings.gradle.kts": true, "build.gradle": true,
	"build.gradle.kts": true, "package.json": true, "pyproject.toml": true,
	"cmakelists.txt": true,
}

func rootHasMarker(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		lower := strings.ToLower(entry.Name())
		if strongRootMarkers[lower] || strings.HasSuffix(lower, ".sln") {
			return true, nil
		}
	}
	return false, nil
}

func rootCandidates(start string) ([]string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("import source %s is a symlink; top-level project roots must be real directories", abs)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	var found []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		// Shared sticky directories such as /tmp are containment boundaries, not
		// credible project roots. Do not let an unrelated marker there capture
		// an explicitly supplied child tree.
		if dir != abs {
			if info, statErr := os.Stat(dir); statErr == nil && info.Mode()&os.ModeSticky != 0 {
				break
			}
		}
		if dir == string(filepath.Separator) {
			break
		}
		has, err := rootHasMarker(dir)
		if err != nil {
			return nil, err
		}
		if has {
			found = append(found, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return found, nil
}

func prettyLanguage(value string) string {
	labels := map[string]string{"java": "Java", "kotlin": "Kotlin", "c": "C", "cpp": "C++", "csharp": "C#", "go": "Go", "rust": "Rust", "python": "Python", "javascript": "JavaScript", "typescript": "TypeScript", "lua": "Lua", "shell": "Shell", "html": "HTML", "css": "CSS", "sql": "SQL", "assembly": "Assembly"}
	if label := labels[value]; label != "" {
		return label
	}
	return value
}
