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
	"strings"
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
	for label, unsafe := range map[string]string{
		"filesystem root": string(filepath.Separator), "home directory": paths.Home,
	} {
		if path == filepath.Clean(unsafe) {
			return fmt.Errorf("refusing to use %s as a project", label)
		}
	}
	for name, store := range cfg.Stores {
		for label, unsafe := range map[string]string{"store root": store.Root, "projects root": filepath.Join(store.Root, "projects"), "tag-view root": filepath.Join(store.Root, "by-tag"), "language-view root": filepath.Join(store.Root, "by-language")} {
			if path == filepath.Clean(unsafe) {
				return fmt.Errorf("refusing to use %s %s for store %q as a project", name, label, name)
			}
		}
	}
	return nil
}

type MountProvider interface {
	IsMount(path string) (bool, error)
}

type ProcMountProvider struct{ Path string }

func (p ProcMountProvider) IsMount(path string) (bool, error) {
	if runtime.GOOS != "linux" {
		return false, errors.New("mount verification is supported only on Linux")
	}
	mountInfo := p.Path
	if mountInfo == "" {
		mountInfo = "/proc/self/mountinfo"
	}
	f, err := os.Open(mountInfo)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return mountInfoContains(f, path)
}

func mountInfoContains(r io.Reader, path string) (bool, error) {
	want := filepath.Clean(path)
	s := bufio.NewScanner(r)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 6 {
			continue
		}
		mountPoint := unescapeMountField(fields[4])
		if filepath.Clean(mountPoint) == want {
			return true, nil
		}
	}
	return false, s.Err()
}

func unescapeMountField(s string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(s)
}

func storeAvailable(cfg Config, name string, mounts MountProvider) error {
	store, ok := cfg.Stores[name]
	if !ok {
		return fmt.Errorf("unknown store %q", name)
	}
	if store.Mount != "" {
		mounted, err := mounts.IsMount(store.Mount)
		if err != nil {
			return fmt.Errorf("cannot verify backing mount %s: %w", store.Mount, err)
		}
		if !mounted {
			return fmt.Errorf("store %q is unavailable: backing mount %s is not mounted", name, store.Mount)
		}
		mountInfo, err := os.Lstat(store.Mount)
		if err != nil {
			return fmt.Errorf("store %q mount boundary is inaccessible: %w", name, err)
		}
		if mountInfo.Mode()&os.ModeSymlink != 0 || !mountInfo.IsDir() {
			return fmt.Errorf("store %q mount boundary %s must be a real directory", name, store.Mount)
		}
		if rootInfo, err := os.Lstat(store.Root); err == nil && rootInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("store %q root %s is a symlink; refusing archive access", name, store.Root)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect store %q root: %w", name, err)
		}
		mountReal, err := filepath.EvalSymlinks(store.Mount)
		if err != nil {
			return fmt.Errorf("resolve store %q mount: %w", name, err)
		}
		ancestor := store.Root
		for {
			if _, statErr := os.Lstat(ancestor); statErr == nil {
				break
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("inspect store %q path: %w", name, statErr)
			}
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				return fmt.Errorf("cannot find existing ancestor for store %q", name)
			}
			ancestor = parent
		}
		ancestorReal, err := filepath.EvalSymlinks(ancestor)
		if err != nil || !pathWithin(ancestorReal, mountReal) {
			return fmt.Errorf("store %q path escapes its verified mount boundary", name)
		}
	}
	return nil
}

func ensureStore(cfg Config, name string, mounts MountProvider) error {
	if err := storeAvailable(cfg, name, mounts); err != nil {
		return err
	}
	root := cfg.Stores[name].Root
	for _, part := range []string{"projects", "by-tag", "by-language"} {
		if err := ensureRealDirectory(filepath.Join(root, part), 0o755); err != nil {
			return fmt.Errorf("create %s store structure: %w", name, err)
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
