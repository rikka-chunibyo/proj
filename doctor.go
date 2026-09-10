package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type doctorReport struct{ errors, warnings, info []string }

func (d *doctorReport) print(a *App) {
	sort.Strings(d.errors)
	sort.Strings(d.warnings)
	sort.Strings(d.info)
	for _, m := range d.errors {
		fmt.Fprintf(a.Out, "ERROR: %s\n", m)
	}
	for _, m := range d.warnings {
		fmt.Fprintf(a.Out, "WARNING: %s\n", m)
	}
	for _, m := range d.info {
		fmt.Fprintf(a.Out, "INFO: %s\n", m)
	}
	fmt.Fprintf(a.Out, "\nDoctor summary: %d error(s), %d warning(s), %d informational item(s). No changes were made.\n", len(d.errors), len(d.warnings), len(d.info))
}

func (a *App) cmdDoctor(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj doctor\nDiagnoses state without modifying config, registry, projects, views, or mounts.")
		return nil
	}
	if len(args) != 0 {
		return errors.New("usage: proj doctor")
	}
	d := doctorReport{}
	if a.ConfigErr != nil {
		d.errors = append(d.errors, "config invalid or requires migration: "+a.ConfigErr.Error())
		d.info = append(d.info, "registry is preserved; replace the config only after preparing /mnt/hdd/archive and the ~/code/archive symlink")
	} else {
		d.info = append(d.info, fmt.Sprintf("config valid (schema version %d)", a.Config.Version))
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		d.errors = append(d.errors, "registry invalid: "+err.Error())
		d.print(a)
		return errors.New("doctor found errors")
	}
	d.info = append(d.info, fmt.Sprintf("registry valid with %d project(s)", len(reg.Projects)))
	if info, err := os.Stat(filepath.Join(a.Paths.StateDir, "registry.lock")); err == nil {
		if info.Mode().Perm()&0o077 != 0 {
			d.warnings = append(d.warnings, "registry lock permissions are broader than 0600")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		d.warnings = append(d.warnings, "cannot inspect registry lock: "+err.Error())
	}
	if a.ConfigErr == nil {
		doctorWorkspace(&d, a, reg)
		doctorArchive(&d, a, reg)
		doctorGlobalViews(&d, a.Config, reg)
	}
	doctorTransactions(&d, a.Paths.StateDir)
	d.print(a)
	if len(d.errors) > 0 {
		return errors.New("doctor found errors")
	}
	return nil
}

func doctorWorkspace(d *doctorReport, a *App, reg Registry) {
	if info, err := os.Lstat(a.Config.WorkspaceRoot); err != nil {
		d.warnings = append(d.warnings, "workspace root unavailable: "+err.Error())
	} else if info.Mode()&os.ModeSymlink != 0 {
		d.errors = append(d.errors, "workspace root is a symlink; a real directory is required: "+a.Config.WorkspaceRoot)
	} else if !info.IsDir() {
		d.errors = append(d.errors, "workspace root is not a directory: "+a.Config.WorkspaceRoot)
	} else {
		d.info = append(d.info, "workspace root: available: "+a.Config.WorkspaceRoot)
	}
	doctorBackingProjects(d, a.Config, "active", true, reg)
}

func doctorArchive(d *doctorReport, a *App, reg Registry) {
	archive := a.Config.Stores["archive"]
	mountRecord, err := validateArchiveMount(a.Config, a.Mounts)
	if err != nil {
		count := 0
		for _, p := range reg.Projects {
			if p.Store == "archive" {
				count++
			}
		}
		d.warnings = append(d.warnings, fmt.Sprintf("%v; %d archived project(s) remain registered and were not classified as missing", err, count))
		doctorArchiveWorkspaceLink(d, a.Config, false)
		return
	}
	d.info = append(d.info, "HDD backing mount: AVAILABLE: "+archive.Mount)
	if mountHasOption(mountRecord, "ro") {
		d.warnings = append(d.warnings, "HDD backing mount is read-only: "+archive.Mount)
	}
	backingOK := true
	info, statErr := os.Lstat(archive.Backing)
	if errors.Is(statErr, os.ErrNotExist) {
		d.errors = append(d.errors, "archive backing directory missing: "+archive.Backing)
		backingOK = false
	} else if statErr != nil {
		d.errors = append(d.errors, "archive backing directory inaccessible: "+statErr.Error())
		backingOK = false
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		d.errors = append(d.errors, "archive backing directory must be a real directory: "+archive.Backing)
		backingOK = false
	} else {
		d.info = append(d.info, "Archive backing store: AVAILABLE: "+archive.Backing)
		if mountHasOption(mountRecord, "ro") {
			d.warnings = append(d.warnings, "archive backing directory is not writable because the HDD mount is read-only")
		} else if err := syscall.Access(archive.Backing, 2|1); err != nil {
			d.warnings = append(d.warnings, "archive backing directory not writable/searchable: "+err.Error())
		} else {
			d.info = append(d.info, "archive backing directory: writable")
		}
	}
	doctorArchiveWorkspaceLink(d, a.Config, true)
	if backingOK {
		doctorBackingProjects(d, a.Config, "archive", true, reg)
	}
	for _, old := range []string{filepath.Join(archive.Mount, "code", "projects"), filepath.Join(archive.Mount, "code", "by-tag"), filepath.Join(archive.Mount, "code", "by-language")} {
		if _, err := os.Lstat(old); err == nil {
			d.warnings = append(d.warnings, "old archive-layout remnant detected: "+old)
		}
	}
}

func doctorArchiveWorkspaceLink(d *doctorReport, cfg Config, backingMounted bool) {
	archive := cfg.Stores["archive"]
	if err := validateArchiveWorkspaceLink(cfg); err != nil {
		d.errors = append(d.errors, "Archive workspace symlink: MISCONFIGURED: "+err.Error())
		return
	}
	status := "HEALTHY"
	if !backingMounted {
		status = "CONFIGURED (target unavailable with HDD)"
	}
	d.info = append(d.info, "Archive workspace symlink: "+status+": "+archive.WorkspacePath+" -> "+archive.Backing)
}

func doctorBackingProjects(d *doctorReport, cfg Config, store string, available bool, reg Registry) {
	if !available {
		return
	}
	root := cfg.Stores[store].Backing
	registered := map[string]bool{}
	for _, p := range reg.Projects {
		if p.Store != store {
			continue
		}
		registered[strings.ToLower(p.Name)] = true
		path, _ := canonicalPath(cfg, p)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			d.errors = append(d.errors, fmt.Sprintf("%s: canonical backing project missing: %s", p.Name, path))
			continue
		}
		if err != nil {
			d.errors = append(d.errors, fmt.Sprintf("%s: cannot inspect canonical project: %v", p.Name, err))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			d.errors = append(d.errors, fmt.Sprintf("%s: canonical entry is a symlink: %s", p.Name, path))
		} else if !info.IsDir() {
			d.errors = append(d.errors, fmt.Sprintf("%s: canonical entry is not a directory: %s", p.Name, path))
		}
		if problem := windowsNameProblem(p.Name); problem != "" {
			d.warnings = append(d.warnings, fmt.Sprintf("%s: Windows/Samba-dangerous name %s", p.Name, problem))
		}
		git, kind, err := detectGit(path)
		if err == nil && git && (kind == "git-file/worktree-style" || kind == "main-repo-with-linked-worktrees") {
			d.warnings = append(d.warnings, fmt.Sprintf("%s: Git relocation-sensitive state: %s", p.Name, kind))
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !registered[strings.ToLower(entry.Name())] {
			d.info = append(d.info, fmt.Sprintf("%s: unregistered canonical directory: %s", store, filepath.Join(root, entry.Name())))
		}
	}
}

func doctorGlobalViews(d *doctorReport, cfg Config, reg Registry) {
	for _, kind := range []struct {
		root   string
		values func(Project) []string
	}{{cfg.Views.Tags, func(p Project) []string { return p.Tags }}, {cfg.Views.Languages, func(p Project) []string { return p.Languages }}} {
		expected := map[string]string{}
		for _, p := range reg.Projects {
			logical, _ := logicalPath(cfg, p)
			for _, value := range kind.values(p) {
				link := filepath.Join(kind.root, value, p.Name)
				target, _ := filepath.Rel(filepath.Dir(link), logical)
				expected[link] = target
			}
		}
		rootInfo, rootErr := os.Lstat(kind.root)
		if errors.Is(rootErr, os.ErrNotExist) {
			for link := range expected {
				d.warnings = append(d.warnings, "missing generated link: "+link)
			}
			continue
		}
		if rootErr != nil {
			d.warnings = append(d.warnings, "cannot inspect generated root: "+kind.root)
			continue
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
			d.errors = append(d.errors, "generated root must be a real directory: "+kind.root)
			continue
		}
		seen := map[string]bool{}
		categories, _ := os.ReadDir(kind.root)
		for _, category := range categories {
			cp := filepath.Join(kind.root, category.Name())
			if category.Type()&os.ModeSymlink != 0 || !category.IsDir() {
				d.warnings = append(d.warnings, "unexpected non-directory in generated root: "+cp)
				continue
			}
			links, _ := os.ReadDir(cp)
			for _, entry := range links {
				lp := filepath.Join(cp, entry.Name())
				want, wanted := expected[lp]
				if entry.Type()&os.ModeSymlink == 0 {
					d.warnings = append(d.warnings, "unexpected regular file or directory in generated tree: "+lp)
					continue
				}
				seen[lp] = true
				target, err := os.Readlink(lp)
				if err != nil {
					d.warnings = append(d.warnings, "cannot read generated link: "+lp)
					continue
				}
				resolved := target
				if !filepath.IsAbs(target) {
					resolved = filepath.Join(cp, target)
				}
				if !pathWithin(filepath.Clean(resolved), cfg.WorkspaceRoot) {
					d.errors = append(d.errors, "generated link escapes configured workspace root: "+lp)
				}
				if !wanted {
					d.warnings = append(d.warnings, "stale generated symlink: "+lp)
					continue
				}
				if target != want {
					d.warnings = append(d.warnings, fmt.Sprintf("incorrect generated target at %s (found %q, want %q)", lp, target, want))
				}
			}
		}
		for link := range expected {
			if !seen[link] {
				d.warnings = append(d.warnings, "missing generated link: "+link)
			}
		}
	}
}

func doctorTransactions(d *doctorReport, stateDir string) {
	dir := filepath.Join(stateDir, "transactions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		d.warnings = append(d.warnings, "cannot inspect transaction journals: "+err.Error())
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			d.warnings = append(d.warnings, "unexpected item in transaction directory: "+filepath.Join(dir, entry.Name()))
			continue
		}
		phase := "unknown"
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err == nil {
			s := bufio.NewScanner(f)
			for s.Scan() {
				key, raw, ok := splitAssignment(strings.TrimSpace(stripComment(s.Text())))
				if ok && key == "phase" {
					phase, _ = strconv.Unquote(raw)
					break
				}
			}
			_ = f.Close()
		}
		d.warnings = append(d.warnings, fmt.Sprintf("incomplete relocation transaction %s (phase: %s); inspect source and destination and preserve both if uncertain", strings.TrimSuffix(entry.Name(), ".toml"), phase))
	}
}
