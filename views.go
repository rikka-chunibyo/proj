package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type ViewReport struct {
	Warnings []string
	Missing  []string
}

func reindexStore(cfg Config, storeName string, reg Registry, mounts MountProvider) (ViewReport, error) {
	var report ViewReport
	if err := storeAvailable(cfg, storeName, mounts); err != nil {
		return report, err
	}
	store := cfg.Stores[storeName]
	projectsRoot := filepath.Join(store.Root, "projects")
	if err := ensureRealDirectory(projectsRoot, 0o755); err != nil {
		return report, err
	}
	desiredTag := map[string]map[string]string{}
	desiredLang := map[string]map[string]string{}
	for _, p := range reg.Projects {
		if p.Store != storeName {
			continue
		}
		canonical := filepath.Join(projectsRoot, p.Name)
		info, err := os.Lstat(canonical)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			report.Missing = append(report.Missing, fmt.Sprintf("%s: canonical project missing or invalid; generated views skipped", p.Name))
			continue
		}
		if err := validateProjectName(p.Name); err != nil {
			return report, fmt.Errorf("unsafe project name in view data: %w", err)
		}
		for _, tag := range p.Tags {
			if normalized, err := normalizeTag(tag); err != nil || normalized != tag {
				return report, fmt.Errorf("unsafe tag view component %q", tag)
			}
			addDesired(desiredTag, tag, p.Name, "../../projects/"+p.Name)
		}
		for _, lang := range p.Languages {
			if err := validateMetadataID("language", lang); err != nil {
				return report, err
			}
			addDesired(desiredLang, lang, p.Name, "../../projects/"+p.Name)
		}
	}
	for _, spec := range []struct {
		root    string
		desired map[string]map[string]string
	}{
		{filepath.Join(store.Root, "by-tag"), desiredTag},
		{filepath.Join(store.Root, "by-language"), desiredLang},
	} {
		warnings, err := reconcileViewRoot(spec.root, spec.desired)
		report.Warnings = append(report.Warnings, warnings...)
		if err != nil {
			return report, err
		}
	}
	sort.Strings(report.Warnings)
	sort.Strings(report.Missing)
	return report, nil
}

func addDesired(m map[string]map[string]string, category, name, target string) {
	if m[category] == nil {
		m[category] = map[string]string{}
	}
	m[category][name] = target
}

func reconcileViewRoot(root string, desired map[string]map[string]string) ([]string, error) {
	if err := ensureRealDirectory(root, 0o755); err != nil {
		return nil, err
	}
	var warnings []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, category := range entries {
		categoryPath := filepath.Join(root, category.Name())
		if category.Type()&os.ModeSymlink != 0 || !category.IsDir() {
			warnings = append(warnings, fmt.Sprintf("unexpected non-directory in generated root preserved: %s", categoryPath))
			continue
		}
		links, err := os.ReadDir(categoryPath)
		if err != nil {
			return warnings, err
		}
		for _, link := range links {
			linkPath := filepath.Join(categoryPath, link.Name())
			want, wanted := desired[category.Name()][link.Name()]
			if link.Type()&os.ModeSymlink == 0 {
				warnings = append(warnings, fmt.Sprintf("unexpected non-symlink preserved: %s", linkPath))
				continue
			}
			actual, readErr := os.Readlink(linkPath)
			if !wanted {
				if err := os.Remove(linkPath); err != nil {
					return warnings, err
				}
			} else if readErr != nil || actual != want {
				if err := os.Remove(linkPath); err != nil {
					return warnings, err
				}
				if err := os.Symlink(want, linkPath); err != nil {
					return warnings, err
				}
			}
		}
		if len(desired[category.Name()]) == 0 {
			if err := os.Remove(categoryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				warnings = append(warnings, fmt.Sprintf("generated category not empty; preserved: %s", categoryPath))
			}
		}
	}
	for category, projects := range desired {
		categoryPath := filepath.Join(root, category)
		if info, err := os.Lstat(categoryPath); err == nil && !info.IsDir() {
			warnings = append(warnings, fmt.Sprintf("cannot create generated category; path occupied: %s", categoryPath))
			continue
		}
		if err := os.MkdirAll(categoryPath, 0o755); err != nil {
			return warnings, err
		}
		for name, target := range projects {
			linkPath := filepath.Join(categoryPath, name)
			info, err := os.Lstat(linkPath)
			if errors.Is(err, os.ErrNotExist) {
				if err := os.Symlink(target, linkPath); err != nil {
					return warnings, err
				}
				continue
			}
			if err != nil {
				return warnings, err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				warnings = append(warnings, fmt.Sprintf("cannot create generated link; path occupied: %s", linkPath))
				continue
			}
			actual, err := os.Readlink(linkPath)
			if err != nil {
				return warnings, err
			}
			if actual != target {
				if err := os.Remove(linkPath); err != nil {
					return warnings, err
				}
				if err := os.Symlink(target, linkPath); err != nil {
					return warnings, err
				}
			}
		}
	}
	return warnings, nil
}

func removeProjectViews(cfg Config, p Project) error {
	store := cfg.Stores[p.Store]
	for _, pair := range []struct {
		root   string
		values []string
	}{
		{filepath.Join(store.Root, "by-tag"), p.Tags},
		{filepath.Join(store.Root, "by-language"), p.Languages},
	} {
		if _, err := validateRealDirectory(pair.root); err != nil {
			return err
		}
		for _, value := range pair.values {
			category := filepath.Join(pair.root, value)
			if _, err := validateRealDirectory(category); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			link := filepath.Join(category, p.Name)
			info, err := os.Lstat(link)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if err := os.Remove(link); err != nil {
				return err
			}
			_ = os.Remove(filepath.Dir(link))
		}
	}
	return nil
}
