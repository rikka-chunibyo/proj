package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

const version = "1.2.1"

type App struct {
	Paths           Paths
	Config          Config
	ConfigErr       error
	Mounts          MountProvider
	In              io.Reader
	Out             io.Writer
	Err             io.Writer
	CWD             string
	TTY             bool
	ProgressTTY     bool
	RelocationHooks RelocationHooks
}

func newApp() (*App, error) {
	home := ""
	if current, err := user.Current(); err == nil {
		home = current.HomeDir
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	paths, err := discoverPaths(os.Getenv, home)
	if err != nil {
		return nil, err
	}
	cfg, _, configErr := loadConfig(paths.ConfigFile, home)
	if configErr != nil {
		cfg = defaultConfig(home)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	tty := false
	if info, err := os.Stdin.Stat(); err == nil {
		tty = info.Mode()&os.ModeCharDevice != 0
	}
	progressTTY := false
	if info, err := os.Stderr.Stat(); err == nil {
		progressTTY = info.Mode()&os.ModeCharDevice != 0
	}
	return &App{Paths: paths, Config: cfg, ConfigErr: configErr, Mounts: ProcMountProvider{}, In: os.Stdin, Out: os.Stdout, Err: os.Stderr, CWD: cwd, TTY: tty, ProgressTTY: progressTTY}, nil
}

func (a *App) relocationStatus(operation, name, message string) {
	newRelocationProgress(a.ProgressTTY, a.Err, operation, name).Status(message)
}

func (a *App) reloadRegistry() (Registry, error) { return loadRegistry(a.Paths.RegistryFile) }

func (a *App) beginMutation() (*FileLock, Registry, error) {
	if a.ConfigErr != nil {
		return nil, Registry{}, a.ConfigErr
	}
	if err := ensureConfig(a.Paths.ConfigFile, a.Config, a.Paths.Home); err != nil {
		return nil, Registry{}, fmt.Errorf("create default config: %w", err)
	}
	lock, err := acquireLock(a.Paths.StateDir)
	if err != nil {
		return nil, Registry{}, fmt.Errorf("acquire mutation lock: %w", err)
	}
	reg, err := a.reloadRegistry()
	if err != nil {
		_ = lock.Close()
		return nil, Registry{}, err
	}
	return lock, reg, nil
}

func (a *App) syncViews(reg Registry) bool {
	report, err := reindexViews(a.Config, reg)
	if err != nil {
		fmt.Fprintf(a.Err, "Generated views are incomplete: %v\nRun:\n  proj reindex\n", err)
		return false
	}
	for _, warning := range report.Missing {
		fmt.Fprintf(a.Err, "%s: canonical active project missing; generated views skipped\n", warning)
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(a.Err, "Warning: %s\n", warning)
	}
	return len(report.Warnings) == 0
}

func (a *App) confirm(prompt string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !a.TTY {
		return false, errors.New("confirmation requires an interactive terminal; rerun with --yes")
	}
	fmt.Fprint(a.Out, prompt)
	line, err := bufio.NewReader(a.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func (a *App) Run(args []string) error {
	if len(args) == 0 {
		printHelp(a.Out)
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		printHelp(a.Out)
		return nil
	case "-v", "--version", "version":
		fmt.Fprintf(a.Out, "proj %s\n", version)
		return nil
	}
	if a.ConfigErr != nil {
		metadataOnly := args[0] == "list" || args[0] == "find" || args[0] == "doctor" || args[0] == "tag" && len(args) > 1 && args[1] == "list"
		if !metadataOnly {
			return a.ConfigErr
		}
	}
	switch args[0] {
	case "add":
		return a.cmdAdd(args[1:])
	case "import":
		return a.cmdImport(args[1:])
	case "archive":
		return a.cmdArchiveRestore("archive", args[1:])
	case "restore":
		return a.cmdArchiveRestore("restore", args[1:])
	case "refresh":
		return a.cmdRefresh(args[1:])
	case "tag":
		return a.cmdTag(args[1:])
	case "find":
		return a.cmdFind(args[1:])
	case "list":
		return a.cmdList(args[1:])
	case "path":
		return a.cmdPath(args[1:])
	case "info":
		return a.cmdInfo(args[1:])
	case "remove", "forget", "unregister":
		return a.cmdRemove(args[0], args[1:])
	case "reindex":
		return a.cmdReindex(args[1:])
	case "doctor":
		return a.cmdDoctor(args[1:])
	case "repair":
		return a.cmdRepair(args[1:])
	case "delete", "category", "move":
		return fmt.Errorf("unknown command %q (intentionally not part of proj v1)", args[0])
	default:
		return fmt.Errorf("unknown command %q; run proj --help", args[0])
	}
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `proj manages canonical project directories and generated metadata views.

Usage:
  proj <command> [options]

Commands:
  add <path>                    Register an existing canonical project
  import <path>                 Move an unmanaged project into a store
  archive <project>             Move an active project to archive
  restore <project>             Move an archived project to active
  refresh <project> | --all     Refresh detected metadata
  find <query...>               Fuzzy-search registry metadata
  list                          List registered projects
  path <project>                Print one verified canonical path
  info <project>                Show project metadata and status
  tag list|add|remove           Inspect or update tags
  remove <project>              Unregister without deleting files
  forget / unregister           Aliases for remove
  reindex                       Rebuild generated views safely
  doctor                        Diagnose stores, registry, and transactions
  repair <project> <path>       Record a deliberate manual move or rename

Global options:
  -h, --help                    Show help
  -v, --version                 Show version

Run proj <command> --help for command-specific help.
`)
}

func requireArgs(args []string, count int, usage string) error {
	if len(args) != count {
		return fmt.Errorf("usage: %s", usage)
	}
	return nil
}

func parseValues(args []string, valueFlags map[string]*[]string, boolFlags map[string]*bool, singleFlags map[string]*string) ([]string, error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if ptr, ok := boolFlags[arg]; ok {
			*ptr = true
			continue
		}
		if ptr, ok := valueFlags[arg]; ok {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
			*ptr = append(*ptr, args[i])
			continue
		}
		if ptr, ok := singleFlags[arg]; ok {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
			*ptr = args[i]
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("unknown option %s", arg)
		}
		positional = append(positional, arg)
	}
	return positional, nil
}

func (a *App) pathFromArg(value string) string {
	if value == "." {
		return a.CWD
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(a.CWD, value)
}
