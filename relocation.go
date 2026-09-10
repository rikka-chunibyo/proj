package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Transaction struct {
	ID               string
	ProjectID        string
	Operation        string
	Source           string
	Destination      string
	Temporary        string
	SourceStore      string
	DestinationStore string
	Phase            string
	Started          string
	SourceDevice     uint64
	SourceInode      uint64
}

type manifestEntry struct {
	Kind   string
	Size   int64
	MTime  int64
	Mode   uint32
	Target string
}

type relocationRequest struct {
	Operation   string
	Source      string
	Destination string
	SourceStore string
	DestStore   string
	Project     Project
	Import      bool
}

type RelocationHooks struct {
	ForceCopy            bool
	AfterCopy            func(source, temporary string) error
	BeforeCleanup        func(source, destination string) error
	CheckFreeSpace       func(path string, required int64) error
	CopyTree             func(source, destination string) error
	BeforeRegistryCommit func() error
}

func transactionText(t Transaction) string {
	return fmt.Sprintf("version = 1\nid = %s\nproject_id = %s\noperation = %s\nsource = %s\ndestination = %s\ntemporary = %s\nsource_store = %s\ndestination_store = %s\nphase = %s\nstarted = %s\nsource_device = %d\nsource_inode = %d\n",
		strconv.Quote(t.ID), strconv.Quote(t.ProjectID), strconv.Quote(t.Operation), strconv.Quote(t.Source), strconv.Quote(t.Destination), strconv.Quote(t.Temporary), strconv.Quote(t.SourceStore), strconv.Quote(t.DestinationStore), strconv.Quote(t.Phase), strconv.Quote(t.Started), t.SourceDevice, t.SourceInode)
}

func saveTransaction(stateDir string, t Transaction) error {
	dir := filepath.Join(stateDir, "transactions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, t.ID+".toml"), []byte(transactionText(t)), 0o600)
}

func finishTransaction(stateDir, id string) error {
	dir := filepath.Join(stateDir, "transactions")
	if err := os.Remove(filepath.Join(dir, id+".toml")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func sourceIdentity(path string) (uint64, uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("filesystem identity unavailable")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func scanManifest(root string) (map[string]manifestEntry, int64, []string, error) {
	manifest := map[string]manifestEntry{}
	var size int64
	var externalLinks []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		kind := ""
		var target string
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
			target, err = os.Readlink(path)
			if err != nil {
				return err
			}
			resolved := target
			if !filepath.IsAbs(target) {
				resolved = filepath.Join(filepath.Dir(path), target)
			}
			if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
				resolved = evaluated
			}
			if !pathWithin(filepath.Clean(resolved), root) {
				externalLinks = append(externalLinks, rel+" -> "+target)
			}
		case info.IsDir():
			kind = "directory"
		case info.Mode().IsRegular():
			kind = "file"
			size += info.Size()
		default:
			return fmt.Errorf("unsupported filesystem object %s (%s); sockets, FIFOs, and device nodes cannot be relocated", path, info.Mode().Type())
		}
		manifest[rel] = manifestEntry{Kind: kind, Size: info.Size(), MTime: info.ModTime().UnixNano(), Mode: uint32(info.Mode()), Target: target}
		return nil
	})
	sort.Strings(externalLinks)
	return manifest, size, externalLinks, err
}

func manifestsEqual(a, b map[string]manifestEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for path, entry := range a {
		if b[path] != entry {
			return false
		}
	}
	return true
}

func checkFreeSpace(path string, required int64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return err
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if required > 0 && uint64(required) > available {
		return fmt.Errorf("project needs about %d bytes but destination has only %d bytes available", required, available)
	}
	return nil
}

func copyTree(source, destination string) error {
	return copyTreeWithProgress(source, destination, nil)
}

func copyTreeWithProgress(source, destination string, onBytes func(int64)) error {
	var dirs []struct {
		path  string
		mode  os.FileMode
		mtime time.Time
	}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		to := destination
		if rel != "." {
			to = filepath.Join(destination, rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case info.IsDir():
			if err := os.Mkdir(to, info.Mode().Perm()); err != nil {
				return err
			}
			dirs = append(dirs, struct {
				path  string
				mode  os.FileMode
				mtime time.Time
			}{to, info.Mode().Perm(), info.ModTime()})
			return nil
		case info.Mode().IsRegular():
			return copyRegular(path, to, info, onBytes)
		default:
			return fmt.Errorf("unsupported filesystem object %s (%s)", path, info.Mode().Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(dirs[i].path, dirs[i].mode)
		_ = os.Chtimes(dirs[i].path, dirs[i].mtime, dirs[i].mtime)
	}
	return nil
}

func copyRegular(source, destination string, info os.FileInfo, onBytes func(int64)) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	writer := io.Writer(out)
	if onBytes != nil {
		writer = &progressWriter{writer: out, onBytes: onBytes}
	}
	if _, err = io.Copy(writer, in); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Chmod(destination, info.Mode().Perm()); err != nil {
		return err
	}
	if err = os.Chtimes(destination, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	ok = true
	return nil
}

type progressWriter struct {
	writer  io.Writer
	onBytes func(int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.onBytes(int64(n))
	}
	return n, err
}

func safeRemoveTemporary(path, projectsRoot, txnID string) error {
	if filepath.Dir(filepath.Clean(path)) != filepath.Clean(projectsRoot) || !strings.Contains(filepath.Base(path), ".proj-txn-"+txnID) {
		return fmt.Errorf("refusing unsafe temporary cleanup of %s", path)
	}
	return os.RemoveAll(path)
}

func safeRemoveSource(path string, expectedDev, expectedIno uint64, paths Paths, cfg Config) error {
	if err := validateSafeSource(path, paths, cfg); err != nil {
		return err
	}
	dev, ino, err := sourceIdentity(path)
	if err != nil {
		return err
	}
	if dev != expectedDev || ino != expectedIno {
		return fmt.Errorf("source identity changed (expected device/inode %d/%d, found %d/%d); source preserved", expectedDev, expectedIno, dev, ino)
	}
	return os.RemoveAll(path)
}

func (a *App) relocate(reg *Registry, req relocationRequest) error {
	progress := newRelocationProgress(a.ProgressTTY, a.Err, req.Operation, req.Project.Name)
	defer progress.CleanLine()
	if _, err := validateRealDirectory(req.Source); err != nil {
		return err
	}
	if err := validateSafeSource(req.Source, a.Paths, a.Config); err != nil {
		return err
	}
	if pathWithin(req.Destination, req.Source) || pathWithin(req.Source, req.Destination) {
		return fmt.Errorf("source and destination overlap; refusing unsafe relocation from %s to %s", req.Source, req.Destination)
	}
	if err := storeAvailable(a.Config, req.DestStore, a.Mounts); err != nil {
		return err
	}
	if err := ensureStore(a.Config, req.DestStore, a.Mounts); err != nil {
		return err
	}
	if _, err := os.Lstat(req.Destination); err == nil {
		return fmt.Errorf("destination already exists: %s", req.Destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if collision, err := caseInsensitiveDiskCollision(filepath.Dir(req.Destination), filepath.Base(req.Destination)); err != nil {
		return err
	} else if collision != "" {
		return fmt.Errorf("destination name collides case-insensitively with existing directory %q", collision)
	}
	if req.Project.GitKind == "git-file/worktree-style" {
		return errors.New("relocation refused: top-level .git is a regular file (linked worktree or submodule-style metadata); worktree-aware relocation is not supported in v1")
	}
	if req.Project.GitKind == "main-repo-with-linked-worktrees" {
		return errors.New("relocation refused: the main repository owns linked worktree metadata under .git/worktrees; moving it may break those worktrees")
	}
	progress.Status("Scanning source...")
	before, size, _, err := scanManifest(req.Source)
	if err != nil {
		return err
	}
	dev, ino, err := sourceIdentity(req.Source)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	txn := Transaction{ID: id, ProjectID: req.Project.ID, Operation: req.Operation, Source: req.Source, Destination: req.Destination, SourceStore: req.SourceStore, DestinationStore: req.DestStore, Phase: "prepared", Started: time.Now().UTC().Format(time.RFC3339Nano), SourceDevice: dev, SourceInode: ino}
	if err := saveTransaction(a.Paths.StateDir, txn); err != nil {
		return fmt.Errorf("create transaction journal: %w", err)
	}
	update := func(phase string) error { txn.Phase = phase; return saveTransaction(a.Paths.StateDir, txn) }
	srcStat, err := os.Stat(req.Source)
	if err != nil {
		return err
	}
	dstStat, err := os.Stat(filepath.Dir(req.Destination))
	if err != nil {
		return err
	}
	same := sameDevice(srcStat, dstStat) && !a.RelocationHooks.ForceCopy
	if same {
		progress.Status("Moving atomically on the same filesystem...")
		if err := os.Rename(req.Source, req.Destination); err != nil {
			return fmt.Errorf("atomic relocation failed: %w", err)
		}
		if err := update("destination_finalized"); err != nil {
			return err
		}
		if err := applyRelocationRegistry(reg, req); err != nil {
			_ = os.Rename(req.Destination, req.Source)
			return err
		}
		progress.Status("Updating registry...")
		if err := a.commitRelocationRegistry(*reg); err != nil {
			if rollbackErr := os.Rename(req.Destination, req.Source); rollbackErr != nil {
				_ = update("registry_commit_failed_destination_preserved")
				return fmt.Errorf("registry commit failed and rename rollback failed; both paths require inspection: %v (rollback: %v)", err, rollbackErr)
			}
			_ = update("rolled_back")
			_ = finishTransaction(a.Paths.StateDir, id)
			return fmt.Errorf("registry commit failed; filesystem rename was rolled back: %w", err)
		}
		if err := update("registry_committed"); err != nil {
			return err
		}
		if err := finishTransaction(a.Paths.StateDir, id); err != nil {
			return err
		}
		return nil
	}
	spaceCheck := checkFreeSpace
	if a.RelocationHooks.CheckFreeSpace != nil {
		spaceCheck = a.RelocationHooks.CheckFreeSpace
	}
	if err := spaceCheck(filepath.Dir(req.Destination), size); err != nil {
		_ = finishTransaction(a.Paths.StateDir, id)
		return err
	}
	temp := filepath.Join(filepath.Dir(req.Destination), "."+filepath.Base(req.Destination)+".proj-txn-"+id)
	txn.Temporary = temp
	if err := update("copying"); err != nil {
		return err
	}
	progress.BeginCopy(size)
	copyFn := copyTree
	if a.RelocationHooks.CopyTree != nil {
		copyFn = a.RelocationHooks.CopyTree
		if err := copyFn(req.Source, temp); err != nil {
			return a.copyFailure(progress, temp, req, id, update, err)
		}
	} else if err := copyTreeWithProgress(req.Source, temp, progress.AddBytes); err != nil {
		return a.copyFailure(progress, temp, req, id, update, err)
	}
	progress.FinishCopy()
	progress.Status("Verifying source snapshot...")
	if hook := a.RelocationHooks.AfterCopy; hook != nil {
		if err := hook(req.Source, temp); err != nil {
			return err
		}
	}
	after, _, _, err := scanManifest(req.Source)
	if err != nil {
		return err
	}
	if !manifestsEqual(before, after) {
		_ = safeRemoveTemporary(temp, filepath.Dir(req.Destination), id)
		_ = finishTransaction(a.Paths.StateDir, id)
		return errors.New("source changed materially during copy; original source was preserved and temporary copy removed; retry when the project is idle")
	}
	progress.Status("Finalizing destination...")
	if err := os.Rename(temp, req.Destination); err != nil {
		return fmt.Errorf("finalize copied destination: %w", err)
	}
	txn.Temporary = ""
	if err := update("destination_finalized"); err != nil {
		return err
	}
	if err := applyRelocationRegistry(reg, req); err != nil {
		return err
	}
	progress.Status("Updating registry...")
	if err := a.commitRelocationRegistry(*reg); err != nil {
		_ = update("registry_commit_failed_destination_preserved")
		return fmt.Errorf("destination was finalized but registry commit failed; source and destination were both preserved: %w", err)
	}
	if err := update("registry_committed"); err != nil {
		return err
	}
	if hook := a.RelocationHooks.BeforeCleanup; hook != nil {
		if err := hook(req.Source, req.Destination); err != nil {
			_ = update("cleanup_blocked")
			return err
		}
	}
	progress.Status("Checking source before cleanup...")
	finalManifest, _, _, err := scanManifest(req.Source)
	if err != nil {
		_ = update("cleanup_blocked")
		return fmt.Errorf("registry points to destination, but source could not be rechecked and was preserved: %w", err)
	}
	if !manifestsEqual(before, finalManifest) {
		_ = update("cleanup_blocked_source_changed")
		return errors.New("registry now points to destination, but source changed after copying; both copies were preserved and the transaction journal records required cleanup")
	}
	progress.Status("Removing verified old source...")
	if err := safeRemoveSource(req.Source, dev, ino, a.Paths, a.Config); err != nil {
		_ = update("cleanup_failed")
		return fmt.Errorf("registry now points to destination, but old source was preserved because cleanup was unsafe or failed: %w", err)
	}
	if err := update("source_removed"); err != nil {
		return err
	}
	return finishTransaction(a.Paths.StateDir, id)
}

func (a *App) copyFailure(progress *relocationProgress, temp string, req relocationRequest, id string, update func(string) error, err error) error {
	progress.CleanLine()
	_ = safeRemoveTemporary(temp, filepath.Dir(req.Destination), id)
	_ = update("copy_failed")
	return fmt.Errorf("copy failed; original source remains untouched: %w", err)
}

func (a *App) commitRelocationRegistry(reg Registry) error {
	if hook := a.RelocationHooks.BeforeRegistryCommit; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	return saveRegistry(a.Paths.RegistryFile, reg)
}

func sameDevice(a, b os.FileInfo) bool {
	as, aok := a.Sys().(*syscall.Stat_t)
	bs, bok := b.Sys().(*syscall.Stat_t)
	return aok && bok && as.Dev == bs.Dev
}
func caseInsensitiveDiskCollision(dir, name string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), name) {
			return e.Name(), nil
		}
	}
	return "", nil
}
func applyRelocationRegistry(reg *Registry, req relocationRequest) error {
	if req.Import {
		reg.Projects = append(reg.Projects, req.Project)
		return validateRegistry(*reg)
	}
	for i := range reg.Projects {
		if reg.Projects[i].ID == req.Project.ID {
			reg.Projects[i].Store = req.DestStore
			return nil
		}
	}
	return fmt.Errorf("project %s disappeared from registry during relocation", req.Project.Name)
}

func (a *App) cmdImport(args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintln(a.Out, "Usage: proj import <path> [--store active|archive] [--tag TAG ...] [--exact-root] [--yes|-y]")
		return nil
	}
	store := "active"
	yes := false
	exactRoot := false
	var tags []string
	pos, err := parseValues(args, map[string]*[]string{"--tag": &tags}, map[string]*bool{"--yes": &yes, "-y": &yes, "--exact-root": &exactRoot}, map[string]*string{"--store": &store})
	if err != nil {
		return err
	}
	if err := requireArgs(pos, 1, "proj import <path> [--store active|archive] [--tag TAG ...] [--yes|-y]"); err != nil {
		return err
	}
	if store != "active" && store != "archive" {
		return fmt.Errorf("unknown store %q", store)
	}
	input := a.pathFromArg(pos[0])
	if canonical, _, canonicalErr := canonicalDirectoryForPath(a.Config, a.Mounts, input); canonicalErr == nil {
		reg, e := a.reloadRegistry()
		if e != nil {
			return e
		}
		if p, _, e := resolveProject(reg, a.Config, a.Mounts, canonical); e == nil {
			return fmt.Errorf("%s is already a registered canonical project; import is not needed", p.Name)
		}
		return fmt.Errorf("%s is already in canonical managed storage; use `proj add %s`", canonical, shellQuote(canonical))
	} else if !errors.Is(canonicalErr, errOutsideManaged) {
		return canonicalErr
	}
	var source string
	if exactRoot {
		source, err = exactImportRoot(input)
		if err != nil {
			return err
		}
	} else {
		candidates, candidateErr := rootCandidates(input)
		if candidateErr != nil {
			return candidateErr
		}
		source, err = a.chooseImportRoot(input, candidates)
		if err != nil {
			return err
		}
	}
	if _, err := validateRealDirectory(source); err != nil {
		return err
	}
	if err := validateSafeSource(source, a.Paths, a.Config); err != nil {
		return err
	}
	name := filepath.Base(source)
	if err := validateProjectName(name); err != nil {
		return err
	}
	if problem := windowsNameProblem(name); problem != "" {
		return fmt.Errorf("cannot import project name %q because it %s; no automatic rename is performed", name, problem)
	}
	normalizedTags, err := normalizeTags(tags)
	if err != nil {
		return err
	}
	if store == "archive" {
		if err := archiveReadyForWrite(a.Config, a.Mounts); err != nil {
			return err
		}
	}
	detection, err := detectProject(source)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	p := Project{ID: id, Name: name, Store: store, Languages: detection.Languages, BuildSystems: detection.BuildSystems, Tags: normalizedTags, Git: detection.Git, GitKind: detection.GitKind}
	_, _, links, err := scanManifest(source)
	if err != nil {
		return err
	}
	for _, link := range links {
		fmt.Fprintf(a.Err, "Warning: symlink escapes project root and may change meaning after relocation: %s\n", link)
	}
	destination := filepath.Join(a.Config.Stores[store].Backing, name)
	ok, err := a.confirm(fmt.Sprintf("Import project %q?\n\nFrom:\n  %s\n\nTo:\n  %s\n\nThis will move the complete project into the %s store.\n\nProceed? [y/N] ", name, source, destination, store), yes)
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
	if existing, _, e := findByNameOrID(reg, name); e == nil {
		return fmt.Errorf("cannot import: name %q collides case-insensitively with registered project %q", name, existing.Name)
	}
	req := relocationRequest{Operation: "import", Source: source, Destination: destination, DestStore: store, Project: p, Import: true}
	if err := a.relocate(&reg, req); err != nil {
		return err
	}
	a.relocationStatus("import", name, "Regenerating generated views...")
	viewsOK := a.syncViews(reg)
	fmt.Fprintf(a.Out, "Imported %s into the %s store.\n", name, store)
	displayDestination := destination
	if store == "archive" {
		displayDestination = filepath.Join(a.Config.Stores["archive"].WorkspacePath, name)
	}
	a.printMovedCWD(source, displayDestination)
	if !viewsOK {
		fmt.Fprintln(a.Err, "Project relocation succeeded, but generated views are incomplete.\nRun:\n  proj reindex")
	}
	return nil
}

func exactImportRoot(input string) (string, error) {
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("import source %s is a symlink; top-level project roots must be real directories", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--exact-root requires a directory, but %s is not a directory", abs)
	}
	return filepath.Clean(abs), nil
}

func (a *App) chooseImportRoot(input string, candidates []string) (string, error) {
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	has, err := rootHasMarker(abs)
	if err != nil {
		return "", err
	}
	if has {
		return abs, nil
	}
	if len(candidates) == 0 {
		return abs, nil
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if !a.TTY {
		return "", fmt.Errorf("multiple project roots are plausible (%s); pass the exact project-root directory in a noninteractive invocation", strings.Join(candidates, ", "))
	}
	fmt.Fprintln(a.Out, "Multiple project roots are plausible:")
	for i, c := range candidates {
		fmt.Fprintf(a.Out, "  %d) %s\n", i+1, c)
	}
	fmt.Fprint(a.Out, "Select project root [1, or q to cancel]: ")
	line, err := bufio.NewReader(a.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return candidates[0], nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(candidates) {
		return "", errors.New("no project root selected; no changes made")
	}
	return candidates[n-1], nil
}

func (a *App) cmdArchiveRestore(operation string, args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprintf(a.Out, "Usage: proj %s <project> [--yes|-y]\n", operation)
		return nil
	}
	yes := false
	pos, err := parseValues(args, nil, map[string]*bool{"--yes": &yes, "-y": &yes}, nil)
	if err != nil {
		return err
	}
	if err := requireArgs(pos, 1, "proj "+operation+" <project> [--yes|-y]"); err != nil {
		return err
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		return err
	}
	p, _, err := resolveProject(reg, a.Config, a.Mounts, pos[0])
	if err != nil {
		return err
	}
	sourceStore, destStore := "active", "archive"
	verb := "Archive"
	if operation == "restore" {
		sourceStore, destStore = "archive", "active"
		verb = "Restore"
	}
	if p.Store == destStore {
		fmt.Fprintf(a.Out, "%s is already in the %s store.\n", p.Name, destStore)
		return nil
	}
	if p.Store != sourceStore {
		return fmt.Errorf("%s cannot be %sd from store %q", p.Name, operation, p.Store)
	}
	if err := storeAvailable(a.Config, sourceStore, a.Mounts); err != nil {
		return err
	}
	if destStore == "archive" {
		if err := archiveReadyForWrite(a.Config, a.Mounts); err != nil {
			return err
		}
	}
	source, _ := canonicalPath(a.Config, *p)
	destination := filepath.Join(a.Config.Stores[destStore].Backing, p.Name)
	_, _, links, err := scanManifest(source)
	if err != nil {
		return err
	}
	for _, link := range links {
		fmt.Fprintf(a.Err, "Warning: symlink escapes project root and may change meaning after relocation: %s\n", link)
	}
	ok, err := a.confirm(fmt.Sprintf("%s %q?\n\nFrom:\n  %s\n\nTo:\n  %s\n\nProceed? [y/N] ", verb, p.Name, source, destination), yes)
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
	current, _, err := resolveProject(reg, a.Config, a.Mounts, p.ID)
	if err != nil {
		return err
	}
	oldProject := *current
	req := relocationRequest{Operation: operation, Source: source, Destination: destination, SourceStore: sourceStore, DestStore: destStore, Project: oldProject}
	if err := a.relocate(&reg, req); err != nil {
		return err
	}
	a.relocationStatus(operation, current.Name, "Regenerating generated views...")
	newViews := a.syncViews(reg)
	fmt.Fprintf(a.Out, "%s moved successfully to the %s store.\n", current.Name, destStore)
	displayDestination := destination
	if destStore == "archive" {
		displayDestination = filepath.Join(a.Config.Stores["archive"].WorkspacePath, current.Name)
	}
	a.printMovedCWD(source, displayDestination)
	if !newViews {
		fmt.Fprintln(a.Err, "Project relocation succeeded, but generated views are incomplete.\nRun:\n  proj reindex")
	}
	return nil
}

func (a *App) printMovedCWD(source, destination string) {
	cwd := a.CWD
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	if pathWithin(cwd, source) {
		fmt.Fprintf(a.Out, "\nYour shell was inside the moved project.\n\nNew location:\n  %s\n\nRun:\n  cd %s\n", destination, shellQuote(destination))
	}
}
