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
)

type doctorReport struct {
	errors   []string
	warnings []string
	info     []string
}

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
		fmt.Fprintln(a.Out, "Usage: proj doctor\nDiagnoses state without modifying registry, projects, views, or transactions.")
		return nil
	}
	if len(args) != 0 {
		return errors.New("usage: proj doctor")
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return fmt.Errorf("registry invalid: %w", err)
	}
	d := doctorReport{}
	d.info = append(d.info, fmt.Sprintf("config valid (schema version %d)", a.Config.Version), fmt.Sprintf("registry valid with %d project(s)", len(reg.Projects)))
	if info, err := os.Stat(filepath.Join(a.Paths.StateDir, "registry.lock")); err == nil {
		if info.Mode().Perm()&0o077 != 0 {
			d.warnings = append(d.warnings, "registry lock permissions are broader than 0600")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		d.warnings = append(d.warnings, "cannot inspect registry lock: "+err.Error())
	}
	for _, storeName := range []string{"active", "archive"} {
		store := a.Config.Stores[storeName]
		availableErr := storeAvailable(a.Config, storeName, a.Mounts)
		if availableErr != nil {
			if storeName == "archive" {
				count := 0
				for _, p := range reg.Projects {
					if p.Store == storeName {
						count++
					}
				}
				d.warnings = append(d.warnings, fmt.Sprintf("Archive store: UNAVAILABLE: %v; %d archived project(s) remain registered and could not be verified", availableErr, count))
				continue
			}
			d.errors = append(d.errors, fmt.Sprintf("active store unavailable: %v", availableErr))
			continue
		}
		d.info = append(d.info, fmt.Sprintf("%s store: available", storeName))
		doctorStore(&d, a.Config, storeName, store, reg)
	}
	doctorTransactions(&d, a.Paths.StateDir)
	d.print(a)
	if len(d.errors) > 0 {
		return errors.New("doctor found errors")
	}
	return nil
}

func doctorStore(d *doctorReport, cfg Config, storeName string, store StoreConfig, reg Registry) {
	projectsRoot := filepath.Join(store.Root, "projects")
	if info, err := os.Stat(store.Root); err != nil {
		d.warnings = append(d.warnings, fmt.Sprintf("cannot inspect %s store root permissions: %v", storeName, err))
	} else if info.Mode().Perm()&0o444 == 0 || info.Mode().Perm()&0o222 == 0 {
		d.warnings = append(d.warnings, fmt.Sprintf("%s store root permission bits show no readable or writable class", storeName))
	}
	registered := map[string]Project{}
	for _, p := range reg.Projects {
		if p.Store != storeName {
			continue
		}
		registered[strings.ToLower(p.Name)] = p
		path, _ := canonicalPath(cfg, p)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			d.errors = append(d.errors, fmt.Sprintf("%s: canonical project missing: %s", p.Name, path))
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
		doctorProjectViews(d, store, p)
	}
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.warnings = append(d.warnings, fmt.Sprintf("cannot scan %s projects root: %v", storeName, err))
		}
		return
	}
	for _, entry := range entries {
		if _, ok := registered[strings.ToLower(entry.Name())]; !ok {
			d.info = append(d.info, fmt.Sprintf("%s: unregistered canonical directory: %s", storeName, filepath.Join(projectsRoot, entry.Name())))
		}
	}
	for _, view := range []string{"by-tag", "by-language"} {
		doctorUnexpectedViews(d, filepath.Join(store.Root, view), store.Root)
	}
}

func doctorProjectViews(d *doctorReport, store StoreConfig, p Project) {
	for _, spec := range []struct {
		root   string
		values []string
	}{{"by-tag", p.Tags}, {"by-language", p.Languages}} {
		for _, value := range spec.values {
			path := filepath.Join(store.Root, spec.root, value, p.Name)
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				d.warnings = append(d.warnings, fmt.Sprintf("%s: missing generated link %s", p.Name, path))
				continue
			}
			if err != nil {
				d.warnings = append(d.warnings, fmt.Sprintf("%s: cannot inspect generated link %s: %v", p.Name, path, err))
				continue
			}
			if info.Mode()&os.ModeSymlink == 0 {
				d.warnings = append(d.warnings, fmt.Sprintf("%s: generated path is not a symlink: %s", p.Name, path))
				continue
			}
			target, err := os.Readlink(path)
			if err != nil {
				d.warnings = append(d.warnings, fmt.Sprintf("%s: cannot read link %s", p.Name, path))
				continue
			}
			want := "../../projects/" + p.Name
			if target != want {
				d.warnings = append(d.warnings, fmt.Sprintf("%s: incorrect generated target at %s (found %q, want %q)", p.Name, path, target, want))
			}
		}
	}
}

func doctorUnexpectedViews(d *doctorReport, root, storeRoot string) {
	rootInfo, rootErr := os.Lstat(root)
	if rootErr == nil && rootInfo.Mode()&os.ModeSymlink != 0 {
		d.errors = append(d.errors, "generated root is a symlink: "+root)
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, category := range entries {
		cp := filepath.Join(root, category.Name())
		if category.Type()&os.ModeSymlink != 0 || !category.IsDir() {
			d.warnings = append(d.warnings, "unexpected non-directory in generated root: "+cp)
			continue
		}
		links, _ := os.ReadDir(cp)
		for _, link := range links {
			lp := filepath.Join(cp, link.Name())
			if link.Type()&os.ModeSymlink == 0 {
				d.warnings = append(d.warnings, "unexpected regular file or directory in generated tree: "+lp)
				continue
			}
			target, _ := os.Readlink(lp)
			resolved := target
			if !filepath.IsAbs(target) {
				resolved = filepath.Join(cp, target)
			}
			if !pathWithin(filepath.Clean(resolved), storeRoot) {
				d.errors = append(d.errors, "generated link escapes configured store root: "+lp)
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
