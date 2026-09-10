package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func (a *App) cmdAdd(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj add <path> [--tag <tag> ...]")
		return nil
	}
	var tags []string
	pos, err := parseValues(args, map[string]*[]string{"--tag": &tags}, nil, nil)
	if err != nil {
		return err
	}
	if err := requireArgs(pos, 1, "proj add <path> [--tag <tag> ...]"); err != nil {
		return err
	}
	input := a.pathFromArg(pos[0])
	canonical, store, err := canonicalDirectoryForPath(a.Config, input)
	if err != nil {
		abs, _ := filepath.Abs(input)
		return fmt.Errorf("Detected project:\n  %s\n\nThis project is not inside a configured proj project store.\n\nNo changes made.\n\nTo explicitly move it into managed storage:\n  proj import %s", abs, shellQuote(abs))
	}
	if err := storeAvailable(a.Config, store, a.Mounts); err != nil {
		return err
	}
	name := filepath.Base(canonical)
	if err := validateProjectName(name); err != nil {
		return err
	}
	normalizedTags, err := normalizeTags(tags)
	if err != nil {
		return err
	}
	lock, reg, err := a.beginMutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	if existing, _, err := findByNameOrID(reg, name); err == nil {
		path, _ := canonicalPath(a.Config, *existing)
		if samePath(path, canonical) {
			fmt.Fprintf(a.Out, "%s is already registered. No metadata changed.\nRun `proj refresh %s` to rerun automatic detection.\n", existing.Name, shellQuote(existing.Name))
			return nil
		}
		return fmt.Errorf("cannot register %q: case-insensitive name collision with registered project %q", name, existing.Name)
	}
	detection, err := detectProject(canonical)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	p := Project{ID: id, Name: name, Store: store, Languages: detection.Languages, BuildSystems: detection.BuildSystems, Tags: normalizedTags, Git: detection.Git, GitKind: detection.GitKind}
	reg.Projects = append(reg.Projects, p)
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		return fmt.Errorf("save registry: %w", err)
	}
	if problem := windowsNameProblem(name); problem != "" {
		fmt.Fprintf(a.Err, "Warning: project name %q %s.\n", name, problem)
	}
	a.syncStoreViews(store, reg)
	fmt.Fprintf(a.Out, "Registered %s in the %s store.\n", name, store)
	return nil
}

func normalizeTags(tags []string) ([]string, error) {
	var result []string
	for _, tag := range tags {
		normalized, err := normalizeTag(tag)
		if err != nil {
			return nil, fmt.Errorf("invalid tag %q: %w", tag, err)
		}
		result = append(result, normalized)
	}
	return sortedUnique(result), nil
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!&;|<>*?()[]{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (a *App) cmdRefresh(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj refresh <project> | proj refresh --all")
		return nil
	}
	all := false
	pos, err := parseValues(args, nil, map[string]*bool{"--all": &all}, nil)
	if err != nil {
		return err
	}
	if all && len(pos) != 0 || !all && len(pos) != 1 {
		return errors.New("usage: proj refresh <project> | proj refresh --all")
	}
	lock, reg, err := a.beginMutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	indices := []int{}
	if all {
		for i := range reg.Projects {
			indices = append(indices, i)
		}
	} else {
		_, i, err := resolveProject(reg, a.Config, pos[0])
		if err != nil {
			return err
		}
		indices = append(indices, i)
	}
	updated := 0
	skipped := 0
	affected := map[string]bool{}
	for _, i := range indices {
		p := &reg.Projects[i]
		if err := storeAvailable(a.Config, p.Store, a.Mounts); err != nil {
			if !all {
				return err
			}
			fmt.Fprintf(a.Err, "Warning: skipped %s: %v\n", p.Name, err)
			skipped++
			continue
		}
		path, _ := canonicalPath(a.Config, *p)
		d, err := detectProject(path)
		if err != nil {
			if !all {
				return err
			}
			fmt.Fprintf(a.Err, "Warning: skipped %s: %v\n", p.Name, err)
			skipped++
			continue
		}
		p.Languages, p.BuildSystems, p.Git, p.GitKind = d.Languages, d.BuildSystems, d.Git, d.GitKind
		affected[p.Store] = true
		updated++
	}
	if updated > 0 {
		if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
			return err
		}
	}
	for store := range affected {
		a.syncStoreViews(store, reg)
	}
	if all {
		fmt.Fprintf(a.Out, "Refreshed %d project(s); skipped %d.\n", updated, skipped)
	} else {
		fmt.Fprintf(a.Out, "Refreshed %s.\n", reg.Projects[indices[0]].Name)
	}
	return nil
}

func (a *App) cmdTag(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj tag list [project]\n       proj tag add <project> <tag...>\n       proj tag remove <project> <tag...>")
		return nil
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) > 2 {
			return errors.New("usage: proj tag list [project]")
		}
		if len(args) == 2 {
			p, _, err := resolveProject(reg, a.Config, args[1])
			if err != nil {
				return err
			}
			for _, tag := range p.Tags {
				fmt.Fprintln(a.Out, tag)
			}
			return nil
		}
		counts := map[string]int{}
		for _, p := range reg.Projects {
			for _, tag := range p.Tags {
				counts[tag]++
			}
		}
		tags := keysFromCounts(counts)
		for _, tag := range tags {
			fmt.Fprintf(a.Out, "%-24s %d\n", tag, counts[tag])
		}
		return nil
	case "add", "remove":
		if len(args) < 3 {
			return fmt.Errorf("usage: proj tag %s <project> <tag...>", args[0])
		}
		tags, err := normalizeTags(args[2:])
		if err != nil {
			return err
		}
		lock, reg, err := a.beginMutation()
		if err != nil {
			return err
		}
		defer lock.Close()
		p, i, err := resolveProject(reg, a.Config, args[1])
		if err != nil {
			return err
		}
		if args[0] == "add" {
			reg.Projects[i].Tags = sortedUnique(append(p.Tags, tags...))
		} else {
			remove := map[string]bool{}
			for _, tag := range tags {
				remove[tag] = true
			}
			kept := []string{}
			for _, tag := range p.Tags {
				if !remove[tag] {
					kept = append(kept, tag)
				}
			}
			reg.Projects[i].Tags = kept
		}
		if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Tags updated for %s.\n", p.Name)
		if err := storeAvailable(a.Config, p.Store, a.Mounts); err != nil {
			fmt.Fprintf(a.Err, "Warning: registry updated, but %s views are unavailable. Run `proj reindex` when the store returns.\n", p.Store)
			return nil
		}
		a.syncStoreViews(p.Store, reg)
		return nil
	default:
		return fmt.Errorf("unknown tag command %q", args[0])
	}
}

func keysFromCounts(m map[string]int) []string {
	result := make([]string, 0, len(m))
	for key := range m {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

type filters struct {
	tags, languages, builds []string
	store                   string
}

func parseFilters(args []string, allowQuery bool) ([]string, filters, error) {
	var f filters
	var active, archived bool
	pos, err := parseValues(args, map[string]*[]string{"--tag": &f.tags, "--language": &f.languages, "--build": &f.builds}, map[string]*bool{"--active": &active, "--archived": &archived}, nil)
	if err != nil {
		return nil, f, err
	}
	if active && archived {
		return nil, f, errors.New("--active and --archived are mutually exclusive")
	}
	if active {
		f.store = "active"
	}
	if archived {
		f.store = "archive"
	}
	if !allowQuery && len(pos) > 0 {
		return nil, f, fmt.Errorf("unexpected argument %q", pos[0])
	}
	return pos, f, nil
}

func matchesFilters(p Project, f filters) bool {
	if f.store != "" && p.Store != f.store {
		return false
	}
	for _, v := range f.tags {
		n, e := normalizeTag(v)
		if e != nil || !contains(p.Tags, n) {
			return false
		}
	}
	for _, v := range f.languages {
		if !containsFold(p.Languages, v) {
			return false
		}
	}
	for _, v := range f.builds {
		if !containsFold(p.BuildSystems, v) {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func (a *App) cmdList(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj list [--active|--archived] [--tag TAG] [--language LANG] [--build BUILD]")
		return nil
	}
	_, f, err := parseFilters(args, false)
	if err != nil {
		return err
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return err
	}
	for _, p := range reg.Projects {
		if matchesFilters(p, f) {
			fmt.Fprintf(a.Out, "%-32s %-8s %s\n", p.Name, p.Store, strings.Join(p.Tags, ", "))
		}
	}
	return nil
}

type findResult struct {
	p     Project
	score int
}

func (a *App) cmdFind(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj find <query...> [filters]")
		return nil
	}
	terms, f, err := parseFilters(args, true)
	if err != nil {
		return err
	}
	if len(terms) == 0 {
		return errors.New("usage: proj find <query...> [filters]")
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return err
	}
	var results []findResult
	for _, p := range reg.Projects {
		if !matchesFilters(p, f) {
			continue
		}
		score, ok := scoreProject(a.Config, p, terms)
		if ok {
			results = append(results, findResult{p, score})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		a, b := strings.ToLower(results[i].p.Name), strings.ToLower(results[j].p.Name)
		if a != b {
			return a < b
		}
		return results[i].p.ID < results[j].p.ID
	})
	for _, r := range results {
		fmt.Fprintf(a.Out, "%-32s %-8s %s\n", r.p.Name, r.p.Store, strings.Join(r.p.Tags, ", "))
	}
	return nil
}

func scoreProject(cfg Config, p Project, terms []string) (int, bool) {
	total := 0
	path, _ := canonicalPath(cfg, p)
	metadata := append([]string{p.Store, path}, append(append([]string{}, p.Tags...), append(p.Languages, p.BuildSystems...)...)...)
	for _, term := range terms {
		term = strings.ToLower(term)
		best := fuzzyField(term, strings.ToLower(p.Name), true)
		for _, field := range metadata {
			if s := fuzzyField(term, strings.ToLower(field), false); s > best {
				best = s
			}
		}
		if best == 0 {
			return 0, false
		}
		total += best
	}
	return total, true
}
func fuzzyField(term, field string, name bool) int {
	base := 500
	if name {
		base = 1000
	}
	if field == term {
		return base
	}
	if strings.HasPrefix(field, term) {
		return base - 100
	}
	if strings.Contains(field, term) {
		return base - 200
	}
	if name {
		d := levenshtein(term, field)
		limit := 2
		if len(term) > 8 {
			limit = 3
		}
		if d <= limit {
			return base - 400 - d*10
		}
		if subsequence(term, field) {
			return base - 450
		}
	}
	return 0
}
func subsequence(a, b string) bool {
	i := 0
	for _, r := range b {
		if i < len(a) && byte(r) == a[i] {
			i++
		}
	}
	return i == len(a)
}
func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func (a *App) cmdPath(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj path <project>")
		return nil
	}
	if err := requireArgs(args, 1, "proj path <project>"); err != nil {
		return err
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return err
	}
	p, _, err := resolveProject(reg, a.Config, args[0])
	if err != nil {
		return err
	}
	if err := storeAvailable(a.Config, p.Store, a.Mounts); err != nil {
		return err
	}
	path, _ := canonicalPath(a.Config, *p)
	if _, err := validateRealDirectory(path); err != nil {
		return fmt.Errorf("canonical project unavailable: %w", err)
	}
	fmt.Fprintln(a.Out, path)
	return nil
}

func (a *App) cmdInfo(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj info <project>")
		return nil
	}
	if err := requireArgs(args, 1, "proj info <project>"); err != nil {
		return err
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return err
	}
	p, _, err := resolveProject(reg, a.Config, args[0])
	if err != nil {
		return err
	}
	path, _ := canonicalPath(a.Config, *p)
	status := "available"
	if err := storeAvailable(a.Config, p.Store, a.Mounts); err != nil {
		status = "unavailable/unverified"
	} else if _, err := validateRealDirectory(path); err != nil {
		status = "missing or invalid"
	}
	fmt.Fprintf(a.Out, "%s\n\nID:\n  %s\n\nStore:\n  %s\n\nPath:\n  %s\n\nStatus:\n  %s\n\nLanguages:\n", p.Name, p.ID, p.Store, path, status)
	for _, v := range p.Languages {
		fmt.Fprintf(a.Out, "  %s\n", prettyLanguage(v))
	}
	if len(p.Languages) == 0 {
		fmt.Fprintln(a.Out, "  (none)")
	}
	fmt.Fprintln(a.Out, "\nBuild systems:")
	for _, v := range p.BuildSystems {
		fmt.Fprintf(a.Out, "  %s\n", v)
	}
	if len(p.BuildSystems) == 0 {
		fmt.Fprintln(a.Out, "  (none)")
	}
	fmt.Fprintln(a.Out, "\nTags:")
	for _, v := range p.Tags {
		fmt.Fprintf(a.Out, "  %s\n", v)
	}
	if len(p.Tags) == 0 {
		fmt.Fprintln(a.Out, "  (none)")
	}
	fmt.Fprintf(a.Out, "\nGit:\n  %t (%s)\n\nViews:\n", p.Git, p.GitKind)
	for _, v := range p.Tags {
		fmt.Fprintf(a.Out, "  by-tag/%s/%s\n", v, p.Name)
	}
	for _, v := range p.Languages {
		fmt.Fprintf(a.Out, "  by-language/%s/%s\n", v, p.Name)
	}
	return nil
}

func (a *App) cmdReindex(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj reindex")
		return nil
	}
	if len(args) != 0 {
		return errors.New("usage: proj reindex")
	}
	lock, reg, err := a.beginMutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	fail := false
	for _, store := range []string{"active", "archive"} {
		if err := storeAvailable(a.Config, store, a.Mounts); err != nil {
			fmt.Fprintf(a.Err, "%s store skipped: %v\n", store, err)
			continue
		}
		report, err := reindexStore(a.Config, store, reg, a.Mounts)
		if err != nil {
			fmt.Fprintf(a.Err, "%s store reindex failed: %v\n", store, err)
			fail = true
			continue
		}
		for _, m := range report.Missing {
			fmt.Fprintln(a.Err, m)
		}
		for _, w := range report.Warnings {
			fmt.Fprintf(a.Err, "Warning: %s\n", w)
		}
	}
	if fail {
		return errors.New("one or more available stores could not be reindexed")
	}
	fmt.Fprintln(a.Out, "Generated views reindexed.")
	return nil
}

func (a *App) cmdRemove(alias string, args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintf(a.Out, "Usage: proj %s <project> [--yes|-y]\n", alias)
		return nil
	}
	yes := false
	pos, err := parseValues(args, nil, map[string]*bool{"--yes": &yes, "-y": &yes}, nil)
	if err != nil {
		return err
	}
	if err := requireArgs(pos, 1, "proj "+alias+" <project> [--yes|-y]"); err != nil {
		return err
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return err
	}
	p, _, err := resolveProject(reg, a.Config, pos[0])
	if err != nil {
		return err
	}
	path, _ := canonicalPath(a.Config, *p)
	ok, err := a.confirm(fmt.Sprintf("Unregister %q?\n\nThis will:\n  remove its proj registry entry\n  remove generated views\n\nThis will NOT delete or modify:\n  %s\n\nProceed? [y/N] ", p.Name, path), yes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(a.Out, "No changes made.")
		return nil
	}
	lock, reg, err := a.beginMutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	p, i, err := resolveProject(reg, a.Config, p.ID)
	if err != nil {
		return err
	}
	old := *p
	reg.Projects = append(reg.Projects[:i], reg.Projects[i+1:]...)
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		return err
	}
	if err := storeAvailable(a.Config, old.Store, a.Mounts); err != nil {
		fmt.Fprintf(a.Err, "Warning: registry entry removed, but %s views could not be cleaned. Run `proj reindex` when the store returns.\n", old.Store)
	} else if err := removeProjectViews(a.Config, old); err != nil {
		fmt.Fprintf(a.Err, "Warning: registry entry removed, but generated views are incomplete: %v\nRun:\n  proj reindex\n", err)
	}
	fmt.Fprintf(a.Out, "Unregistered %s. Project files were not deleted or moved.\n", old.Name)
	return nil
}

func (a *App) cmdRepair(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj repair <project> <canonical-path>")
		return nil
	}
	if err := requireArgs(args, 2, "proj repair <project> <canonical-path>"); err != nil {
		return err
	}
	target := a.pathFromArg(args[1])
	canonical, store, err := canonicalDirectoryForPath(a.Config, target)
	if err != nil {
		return fmt.Errorf("repair path must be a real directory directly under a configured projects root; external projects belong via `proj import`: %w", err)
	}
	if filepath.Clean(canonical) != filepath.Clean(target) {
		abs, _ := filepath.Abs(target)
		if filepath.Clean(canonical) != filepath.Clean(abs) {
			return errors.New("repair path must name the canonical project directory itself, not a nested path")
		}
	}
	if err := storeAvailable(a.Config, store, a.Mounts); err != nil {
		return err
	}
	name := filepath.Base(canonical)
	if err := validateProjectName(name); err != nil {
		return err
	}
	lock, reg, err := a.beginMutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	_, i, err := resolveProject(reg, a.Config, args[0])
	if err != nil {
		return err
	}
	for j, p := range reg.Projects {
		if j != i && strings.EqualFold(p.Name, name) {
			return fmt.Errorf("cannot repair: name %q collides case-insensitively with registered project %q", name, p.Name)
		}
	}
	old := reg.Projects[i]
	reg.Projects[i].Name = name
	reg.Projects[i].Store = store
	if err := saveRegistry(a.Paths.RegistryFile, reg); err != nil {
		return err
	}
	if err := storeAvailable(a.Config, old.Store, a.Mounts); err == nil {
		_ = removeProjectViews(a.Config, old)
	}
	a.syncStoreViews(store, reg)
	fmt.Fprintf(a.Out, "Repaired %s: registry now identifies %s in the %s store. Metadata was preserved; run `proj refresh %s` if detection is stale.\n", old.Name, name, store, shellQuote(name))
	return nil
}
