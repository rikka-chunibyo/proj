package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unicode"
)

func pathWithin(path, parent string) bool {
	path, parent = filepath.Clean(path), filepath.Clean(parent)
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func validateProjectName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("name must be a nonempty ordinary directory name")
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, "/\x00") {
		return errors.New("name must be one filesystem path component without path separators")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("name must not contain control characters")
		}
	}
	return nil
}
func validateMetadataID(kind, value string) error {
	if value == "" || value == "." || value == ".." {
		return fmt.Errorf("%s identifier is empty or path-dangerous", kind)
	}
	for i, r := range value {
		allowed := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
		if !allowed || i == 0 && !(r >= 'a' && r <= 'z') {
			return fmt.Errorf("invalid %s identifier %q", kind, value)
		}
	}
	return nil
}

var windowsReserved = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$`)

func windowsNameProblem(name string) string {
	if strings.ContainsAny(name, `<>:"/\\|?*`) {
		return "contains a character forbidden by Windows"
	}
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return "has a trailing space or period, which Windows does not preserve safely"
	}
	if windowsReserved.MatchString(name) {
		return "is a reserved Windows device name"
	}
	return ""
}
func normalizeTag(tag string) (string, error) {
	if strings.ContainsAny(tag, "/\\\x00") {
		return "", errors.New("tag must not contain path separators or NUL")
	}
	for _, r := range tag {
		if unicode.IsControl(r) {
			return "", errors.New("tag must not contain control characters")
		}
	}
	parts := strings.Fields(strings.TrimSpace(tag))
	value := strings.ToLower(strings.Join(parts, "-"))
	if value == "" || value == "." || value == ".." {
		return "", errors.New("tag is empty or path-dangerous after normalization")
	}
	for i, r := range value {
		allowed := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == '+'
		if !allowed {
			return "", fmt.Errorf("tag contains unsupported character %q", r)
		}
		if i == 0 && !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return "", errors.New("tag must begin with a letter or digit")
		}
	}
	return value, nil
}

func validateSafeSource(path string, paths Paths, cfg Config) error {
	path = filepath.Clean(path)
	protected := map[string]string{"filesystem root": string(filepath.Separator), "home directory": paths.Home, "workspace root": cfg.WorkspaceRoot, "active backing root": cfg.Stores["active"].Backing, "archive backing root": cfg.Stores["archive"].Backing, "archive workspace mount": cfg.Stores["archive"].WorkspacePath, "tag-view root": cfg.Views.Tags, "language-view root": cfg.Views.Languages}
	for label, unsafe := range protected {
		if unsafe != "" && path == filepath.Clean(unsafe) {
			return fmt.Errorf("refusing to use %s as a project", label)
		}
	}
	return nil
}

type MountRecord struct {
	Device, Root, MountPoint, FSType, Source string
	Options                                  []string
}
type MountProvider interface {
	IsMount(path string) (bool, error)
}
type MountTopologyProvider interface{ MountRecords() ([]MountRecord, error) }
type ProcMountProvider struct{ Path string }

func (p ProcMountProvider) mountInfoPath() string {
	if p.Path != "" {
		return p.Path
	}
	return "/proc/self/mountinfo"
}
func (p ProcMountProvider) IsMount(path string) (bool, error) {
	records, err := p.MountRecords()
	if err != nil {
		return false, err
	}
	_, ok := exactMountRecord(records, path)
	return ok, nil
}
func (p ProcMountProvider) MountRecords() ([]MountRecord, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("mount verification is supported only on Linux")
	}
	f, err := os.Open(p.mountInfoPath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountInfo(f)
}

func parseMountInfo(r io.Reader) ([]MountRecord, error) {
	var records []MountRecord
	s := bufio.NewScanner(r)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		separator := -1
		for i, v := range fields {
			if v == "-" {
				separator = i
				break
			}
		}
		if len(fields) < 10 || separator < 6 || separator+3 > len(fields) {
			continue
		}
		records = append(records, MountRecord{Device: fields[2], Root: unescapeMountField(fields[3]), MountPoint: unescapeMountField(fields[4]), Options: strings.Split(fields[5], ","), FSType: fields[separator+1], Source: unescapeMountField(fields[separator+2])})
	}
	return records, s.Err()
}
func mountInfoContains(r io.Reader, path string) (bool, error) {
	records, err := parseMountInfo(r)
	if err != nil {
		return false, err
	}
	_, ok := exactMountRecord(records, path)
	return ok, nil
}
func unescapeMountField(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
func exactMountRecord(records []MountRecord, path string) (MountRecord, bool) {
	want := filepath.Clean(path)
	for _, record := range records {
		if filepath.Clean(record.MountPoint) == want {
			return record, true
		}
	}
	return MountRecord{}, false
}
func coveringMountRecord(records []MountRecord, path string) (MountRecord, bool) {
	path = filepath.Clean(path)
	matches := append([]MountRecord(nil), records...)
	sort.Slice(matches, func(i, j int) bool {
		return len(filepath.Clean(matches[i].MountPoint)) > len(filepath.Clean(matches[j].MountPoint))
	})
	for _, record := range matches {
		if pathWithin(path, record.MountPoint) {
			return record, true
		}
	}
	return MountRecord{}, false
}
func mountHasOption(record MountRecord, want string) bool {
	for _, option := range record.Options {
		if option == want {
			return true
		}
	}
	return false
}
func topology(mounts MountProvider) ([]MountRecord, error) {
	provider, ok := mounts.(MountTopologyProvider)
	if !ok {
		return nil, errors.New("mount provider cannot validate bind-mount topology")
	}
	return provider.MountRecords()
}

func validateArchiveMount(cfg Config, mounts MountProvider) (MountRecord, error) {
	archive := cfg.Stores["archive"]
	mounted, err := mounts.IsMount(archive.Mount)
	if err != nil {
		return MountRecord{}, fmt.Errorf("cannot verify HDD backing mount %s: %w", archive.Mount, err)
	}
	if !mounted {
		return MountRecord{}, fmt.Errorf("HDD backing mount unavailable: %s is not mounted", archive.Mount)
	}
	info, err := os.Lstat(archive.Mount)
	if err != nil {
		return MountRecord{}, fmt.Errorf("HDD backing mount inaccessible: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return MountRecord{}, fmt.Errorf("HDD backing mount %s must be a real directory", archive.Mount)
	}
	records, err := topology(mounts)
	if err != nil {
		return MountRecord{}, err
	}
	record, ok := exactMountRecord(records, archive.Mount)
	if !ok {
		return MountRecord{}, fmt.Errorf("HDD backing mount unavailable: %s is not an exact mount point", archive.Mount)
	}
	return record, nil
}

func archiveBackingAvailable(cfg Config, mounts MountProvider) error {
	hddMount, err := validateArchiveMount(cfg, mounts)
	if err != nil {
		return err
	}
	archive := cfg.Stores["archive"]
	info, err := os.Lstat(archive.Backing)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("archive backing directory missing: %s", archive.Backing)
	}
	if err != nil {
		return fmt.Errorf("archive backing directory inaccessible: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive backing %s is a symlink; a real directory is required", archive.Backing)
	}
	if !info.IsDir() {
		return fmt.Errorf("archive backing %s must be a real directory", archive.Backing)
	}
	mountReal, err := filepath.EvalSymlinks(archive.Mount)
	if err != nil {
		return err
	}
	backingReal, err := filepath.EvalSymlinks(archive.Backing)
	if err != nil {
		return err
	}
	if !pathWithin(backingReal, mountReal) {
		return errors.New("archive backing path escapes the verified HDD mount")
	}
	records, err := topology(mounts)
	if err != nil {
		return err
	}
	covering, ok := coveringMountRecord(records, archive.Backing)
	if !ok {
		return fmt.Errorf("cannot determine filesystem backing for %s", archive.Backing)
	}
	expectedRel, err := filepath.Rel(hddMount.MountPoint, archive.Backing)
	if err != nil {
		return err
	}
	actualRel, err := filepath.Rel(covering.MountPoint, archive.Backing)
	if err != nil {
		return err
	}
	expectedRoot := filepath.Clean(filepath.Join(hddMount.Root, expectedRel))
	actualRoot := filepath.Clean(filepath.Join(covering.Root, actualRel))
	if covering.Device != hddMount.Device || actualRoot != expectedRoot {
		return fmt.Errorf("archive backing path %s is not on the expected HDD backing filesystem", archive.Backing)
	}
	if err := syscall.Access(archive.Backing, 4|1); err != nil {
		return fmt.Errorf("archive backing directory is not readable/searchable: %w", err)
	}
	return nil
}

func archiveBackingWritable(cfg Config, mounts MountProvider) error {
	record, err := validateArchiveMount(cfg, mounts)
	if err != nil {
		return err
	}
	if mountHasOption(record, "ro") {
		return fmt.Errorf("HDD backing mount is read-only: %s", cfg.Stores["archive"].Mount)
	}
	if err := archiveBackingAvailable(cfg, mounts); err != nil {
		return err
	}
	records, err := topology(mounts)
	if err != nil {
		return err
	}
	if backingRecord, ok := coveringMountRecord(records, cfg.Stores["archive"].Backing); ok && mountHasOption(backingRecord, "ro") {
		return fmt.Errorf("archive backing filesystem is read-only: %s", cfg.Stores["archive"].Backing)
	}
	if err := syscall.Access(cfg.Stores["archive"].Backing, 2|1); err != nil {
		return fmt.Errorf("archive backing directory is not writable/searchable: %w", err)
	}
	return nil
}

func archiveWorkspaceAvailable(cfg Config, mounts MountProvider) error {
	if err := archiveBackingAvailable(cfg, mounts); err != nil {
		return err
	}
	return validateArchiveWorkspaceMount(cfg, mounts)
}

// validateArchiveWorkspaceMount checks only the logical access mount. Keeping
// it independent from backing-directory existence lets doctor report both
// system faults separately instead of collapsing one into the other.
func validateArchiveWorkspaceMount(cfg Config, mounts MountProvider) error {
	archive := cfg.Stores["archive"]
	info, err := os.Lstat(archive.WorkspacePath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("archive workspace bind mount missing: %s", archive.WorkspacePath)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive workspace path %s is a symlink; a bind mount is required", archive.WorkspacePath)
	}
	if !info.IsDir() {
		return fmt.Errorf("archive workspace path %s is not a directory", archive.WorkspacePath)
	}
	records, err := topology(mounts)
	if err != nil {
		return err
	}
	logical, ok := exactMountRecord(records, archive.WorkspacePath)
	if !ok {
		return fmt.Errorf("archive workspace path is not a mount point: %s", archive.WorkspacePath)
	}
	hddMount, ok := exactMountRecord(records, archive.Mount)
	if !ok {
		return fmt.Errorf("HDD backing mount unavailable: %s is not an exact mount point", archive.Mount)
	}
	rel, err := filepath.Rel(hddMount.MountPoint, archive.Backing)
	if err != nil {
		return err
	}
	expectedRoot := filepath.Clean(filepath.Join(hddMount.Root, rel))
	if logical.Device != hddMount.Device || filepath.Clean(logical.Root) != expectedRoot {
		return fmt.Errorf("archive workspace mount points to the wrong backing directory: expected %s to expose %s", archive.WorkspacePath, archive.Backing)
	}
	return nil
}

func archiveReadyForWrite(cfg Config, mounts MountProvider) error {
	if err := archiveBackingWritable(cfg, mounts); err != nil {
		return err
	}
	return archiveWorkspaceAvailable(cfg, mounts)
}
func storeAvailable(cfg Config, name string, mounts MountProvider) error {
	switch name {
	case "active":
		return nil
	case "archive":
		return archiveBackingAvailable(cfg, mounts)
	default:
		return fmt.Errorf("unknown store %q", name)
	}
}
func ensureStore(cfg Config, name string, mounts MountProvider) error {
	switch name {
	case "active":
		return ensureWorkspace(cfg)
	case "archive":
		return archiveReadyForWrite(cfg, mounts)
	default:
		return fmt.Errorf("unknown store %q", name)
	}
}
func ensureWorkspace(cfg Config) error {
	for _, path := range []string{cfg.WorkspaceRoot, cfg.Stores["active"].Backing, cfg.Views.Tags, cfg.Views.Languages} {
		if err := ensureRealDirectory(path, 0o755); err != nil {
			return fmt.Errorf("create workspace structure: %w", err)
		}
	}
	return nil
}
func ensureRealDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to use it as a managed directory", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	_, err = validateRealDirectory(path)
	return err
}
func validateRealDirectory(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; canonical project roots must be real directories", path)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	return info, nil
}
