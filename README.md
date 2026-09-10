# proj

`proj` is a Linux project-management CLI built around one deliberately simple rule: every project has one real canonical directory, while tags and detected languages are disposable symlink views.

```text
~/code/
├── projects/       real active project directories
├── by-tag/         proj-managed relative symlinks
└── by-language/    proj-managed relative symlinks

/mnt/hdd/code/
├── projects/       real archived project directories
├── by-tag/         proj-managed relative symlinks
└── by-language/    proj-managed relative symlinks
```

The archive is a storage tier, not a separate project namespace. Names are globally unique across both stores using case-insensitive comparison.

## Installation

Build a self-contained binary with Go 1.27 or newer:

```bash
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o "$HOME/.local/bin/proj" .
```

Or copy the already-built artifact from this source tree:

```bash
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/proj "$HOME/.local/bin/proj"
```

Ensure `~/.local/bin` is in `PATH`, then run:

```bash
proj --help
proj --version
```

Neither build method requires root. The program has no runtime dependencies, daemon, database server, or network component.

## Everyday commands

```bash
# Register a real directory already under <store>/projects/.
proj add ~/code/projects/Matrix --tag minecraft --tag client

# Explicitly relocate an unmanaged project into active storage.
proj import ~/Downloads/PulseMonitor --tag hardware --tag tooling

# Force a markerless directory to be the root, even inside a larger repo.
proj import ~/repo/tools/small-script --exact-root

# Move between storage tiers.
proj archive Matrix
proj restore matrix

# Refresh only automatically detected metadata.
proj refresh Matrix
proj refresh --all

# Search and inspect.
proj find java minecraft
proj find pulsmon
proj list --tag embedded --language cpp
proj info PulseMonitor
proj path matrix

# Human tags are normalized and remain independent of physical placement.
proj tag add Matrix "Reverse Engineering" bytecode
proj tag remove Matrix bytecode
proj tag list
proj tag list Matrix

# Unregister only. Project data is never deleted or moved by this command.
proj remove Matrix

# Recreate disposable views and diagnose state.
proj reindex
proj doctor

# Record a deliberate manual canonical move or rename; no data is moved.
proj repair OldName ~/code/projects/NewName
```

Direct project resolution accepts an exact stable ID, a case-insensitive exact name, `.`, or an appropriate path. It never fuzzy-matches. Fuzzy typo matching is confined to `proj find`.

`proj import`, `archive`, `restore`, and `remove` prompt before their destructive or identity-changing step. In scripts or other noninteractive input, explicitly pass `--yes`/`-y`.

## Import root discovery

Without `--exact-root`, import behaves as follows:

1. If the supplied directory has a strong project marker, use it.
2. Otherwise, search upward for credible roots.
3. If several roots are materially plausible, ask interactively; noninteractive use fails safely.
4. If no marker exists, use the supplied directory.

`--exact-root` disables only the upward search. All path, symlink, name, collision, Git-worktree, and relocation safety checks still apply.

## Shell navigation helper

A Go child process cannot change its parent shell directory. Add this helper to the shell configuration:

```bash
pcd() {
    local target
    target="$(proj path "$@")" || return
    cd "$target"
}
```

Then `pcd Matrix` and `pcd matrix` both navigate to the verified canonical directory. `proj path` prints only the path on stdout and fails if the store or directory is unavailable.

## Persistent state

`proj` honors XDG base-directory environment variables:

| Purpose | XDG path | Fallback |
| --- | --- | --- |
| Configuration | `$XDG_CONFIG_HOME/proj/config.toml` | `~/.config/proj/config.toml` |
| Registry | `$XDG_DATA_HOME/proj/projects.toml` | `~/.local/share/proj/projects.toml` |
| Lock and recovery state | `$XDG_STATE_HOME/proj/` | `~/.local/state/proj/` |

The default configuration is created only by a command that needs persistent mutation:

```toml
version = 1

[stores.active]
root = "~/code"

[stores.archive]
root = "/mnt/hdd/code"
mount = "/mnt/hdd"
```

The registry is deterministic, human-readable TOML. It stores stable ID, name, store, languages, build systems, tags, and Git state. It does not store an independent absolute project path; the path is always derived from store plus name.

Registry mutations hold an exclusive Linux file lock and use flushed same-filesystem atomic replacement with restrictive permissions.

## Filesystem and recovery safety

- `add` registers only and never moves data.
- `import`, `archive`, and `restore` share one relocation engine and never overwrite a destination.
- `remove`, `forget`, and `unregister` remove only registry metadata and accessible generated links. They never delete canonical project files.
- Canonical project entries and import roots must be real directories, not symlinks. Internal symlinks are preserved without dereferencing.
- Sockets, FIFOs, and device nodes cause relocation refusal.
- A top-level `.git` file and a main repository with `.git/worktrees/` cause relocation refusal in v1.
- Cross-filesystem moves stage on the destination filesystem, preflight apparent size/free space, snapshot inexpensive source metadata, recheck for changes, finalize atomically, commit the registry, verify source device/inode, and only then remove the old source.
- If source changes, identity changes, commit fails, or cleanup becomes uncertain, `proj` prefers preserving duplicate copies over deleting data.
- Relocation journals live under `transactions/` in the state directory. `proj doctor` reports incomplete journals but never chooses or deletes a copy automatically.
- The archive root is never created or written until its configured mount is an exact verified mount boundary. Existing archive-root symlinks are refused.
- `by-tag/` and `by-language/` are exclusively generated. Reindex removes only stale symlinks and empty directories; unexpected regular files or nonempty directories are preserved and reported.

If generated view updates fail after an authoritative registry mutation or completed relocation, run `proj reindex` after resolving the reported filesystem issue.

## Detection

Source scanning ignores common generated, cache, VCS, environment, dependency, and vendor directories and does not follow symlinks. V1 detects substantive Java, Kotlin, C, C++, C#, Go, Rust, Python, JavaScript, TypeScript, Lua, shell, HTML, CSS, SQL, and assembly source. Data/document formats such as JSON, YAML, TOML, XML, and Markdown are not language classifications by mere presence.

Build metadata is tracked separately for Maven, Gradle, Cargo, Go Modules, npm, pnpm, Yarn, CMake, and MSBuild.

`refresh` owns scanning. `reindex` never scans or mutates project contents.

## V1 limitations and deferred scope

- Linux is required for mountinfo verification and file locking.
- Hard-link topology, ACLs, and extended attributes are not preserved; normal Unix modes, executable bits, symlinks, and modification times are preserved.
- Sparse-file optimization and full-file hashing are intentionally not part of relocation verification.
- Git linked-worktree relocation and overrides are intentionally unsupported.
- Shell completion is deferred.
- There is no project deletion command, category system, primary tag, daemon, watcher, synchronization, IDE launcher, project generator, Git hosting automation, or web UI.

## Development checks

All tests use temporary homes, stores, mounts, config, registry, and state paths. They do not access the real `~/code`, `/mnt/hdd/code`, or user XDG state.

```bash
gofmt -w *.go
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/proj .
```
