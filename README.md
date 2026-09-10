# proj

`proj` is a Linux project-management CLI built around one rule: every project
has one real backing directory, while tags and detected languages are global,
disposable symlink views.

```text
~/code/
├── projects/       real active projects on SSD
├── archive/        bind mount exposing archived projects
├── by-tag/         one global proj-managed view
└── by-language/    one global proj-managed view

/mnt/hdd/
└── archive/
    └── code/        real archived projects on HDD
```

The archive is a storage tier, not a second namespace. Project names remain
globally unique across active and archive stores, case-insensitively.

## Installation

Build a self-contained binary with Go 1.27 or newer:

```bash
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o "$HOME/.local/bin/proj" .
```

Or install the artifact built in this source tree:

```bash
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/proj "$HOME/.local/bin/proj"
```

No command in `proj` invokes `sudo`, mounts filesystems, or requires a daemon,
database, runtime, or network service.

## One-time workspace and archive setup

The active workspace can be initialized as the normal user:

```bash
mkdir -p "$HOME/code/projects" "$HOME/code/archive" \
  "$HOME/code/by-tag" "$HOME/code/by-language"
```

Before configuring the archive, make sure `/mnt/hdd` is the intended mounted
filesystem and is writable. Fixing a read-only VirtioFS mount is host/system
configuration and is intentionally outside `proj`.

Create the physical archive directory with ownership for the current user:

```bash
sudo install -d -o "$(id -un)" -g "$(id -gn)" -m 0755 /mnt/hdd/archive/code
```

Add this bind mount to `/etc/fstab` (fstab does not expand `~`, so use the
absolute home path):

```fstab
/mnt/hdd/archive/code /home/rikka/code/archive none bind,nofail,x-systemd.requires-mounts-for=/mnt/hdd 0 0
```

Then mount and verify it:

```bash
sudo mount /home/rikka/code/archive
findmnt --target /mnt/hdd
findmnt --target /home/rikka/code/archive
proj doctor
```

`proj archive` and `proj import --store archive` refuse to start unless the HDD
mount, writable physical backing directory, and correct bind mount are all
healthy. `proj restore` and archived `proj refresh` deliberately use the
physical backing path and remain recovery-capable if only the bind mount is
broken. `proj` never repairs or remounts system filesystems itself.

The prior archive layout can be removed only after checking it contains no
project data. Empty legacy directories can be removed safely with `rmdir`:

```bash
sudo rmdir /mnt/hdd/code/projects /mnt/hdd/code/by-tag \
  /mnt/hdd/code/by-language /mnt/hdd/code
```

`rmdir` refuses nonempty directories; do not substitute a recursive deletion
without inspecting the contents.

## Configuration and compatibility

`proj` honors XDG base-directory variables:

| Purpose | XDG path | Fallback |
| --- | --- | --- |
| Configuration | `$XDG_CONFIG_HOME/proj/config.toml` | `~/.config/proj/config.toml` |
| Registry | `$XDG_DATA_HOME/proj/projects.toml` | `~/.local/share/proj/projects.toml` |
| Lock/recovery state | `$XDG_STATE_HOME/proj/` | `~/.local/state/proj/` |

The v2 configuration is:

```toml
version = 2

[workspace]
root = "~/code"

[stores.active]
backing = "~/code/projects"

[stores.archive]
backing = "/mnt/hdd/archive/code"
mount = "/mnt/hdd"
workspace_path = "~/code/archive"

[views]
tags = "~/code/by-tag"
languages = "~/code/by-language"
```

The registry remains schema v1 and is compatible with existing entries. It
stores only stable ID, name, store, languages, build systems, tags, and Git
state; paths are derived rather than duplicated. An old two-tree config is
detected and mutating commands are refused with an upgrade diagnostic. `proj`
does not move old archive data during a config upgrade.

Registry mutations hold an exclusive Linux file lock and use flushed,
same-filesystem atomic replacement with restrictive permissions.

## Everyday commands

```bash
# Register without moving an existing active project.
proj add ~/code/projects/Matrix --tag minecraft --tag client

# Explicit import; --exact-root disables upward project-root discovery.
proj import ~/Downloads/PulseMonitor --tag hardware --tag tooling
proj import ~/repo/tools/small-script --exact-root
proj import ~/old/OldPlugin --store archive

# Explicit storage-tier changes.
proj archive Matrix
proj restore matrix

# Refresh automatic metadata; tags are preserved.
proj refresh Matrix
proj refresh --all

# Search, filter, inspect, and navigate.
proj find java minecraft
proj find pulsmon
proj list --tag embedded --language cpp
proj info PulseMonitor
proj path matrix

# Tags need no separate initialization.
proj tag add Matrix "Reverse Engineering" bytecode
proj tag remove Matrix bytecode
proj tag list
proj tag list Matrix

# Unregister only; project files remain untouched.
proj remove Matrix

# Reconstruct global views, diagnose, or record a manual canonical move.
proj reindex
proj doctor
proj repair OldName ~/code/projects/NewName
```

Direct resolution accepts an exact stable ID, a case-insensitive exact name,
`.`, or an appropriate managed path. It never fuzzy-matches; typo matching is
confined to `proj find`. Generated view paths resolve to their registered
project, while unrelated external symlinks do not acquire project identity.

For archived projects, `proj path` prints `~/code/archive/<name>` only when the
bind mount is verified and navigable. `proj info` shows that logical path and
the physical `/mnt/hdd/archive/code/<name>` backing path. List/find/tag metadata
continue to work when the archive is offline.

## Global generated views

Both stores appear together under the SSD-resident global views:

```text
~/code/by-tag/minecraft/Matrix    -> ../../projects/Matrix
~/code/by-tag/minecraft/OldPlugin -> ../../archive/OldPlugin
```

Archiving and restoring retarget links; they do not relocate view trees. When
the HDD is offline, registered archive links deliberately remain and may dangle.
Registry membership—not target availability—determines whether an archived link
is stale. Unexpected regular files or nonempty user directories in generated
trees are reported and preserved.

`by-tag/` and `by-language/` are exclusively `proj`-managed. They contain no
canonical project data and can be reconstructed with `proj reindex`.

## Import discovery and relocation progress

Without `--exact-root`, import uses the supplied directory immediately when it
has a strong marker; otherwise it searches upward for the nearest credible
root. If several roots are materially plausible it asks on a TTY and fails
safely when noninteractive. If none exists, it uses the supplied directory.

On an interactive terminal, cross-filesystem relocations display one live,
byte-based progress line with percentage, transferred/total size, throughput,
and ETA. Scanning, verification, finalization, registry, cleanup, and view phases
are reported concisely. Non-TTY output has no animation or control sequences,
and same-filesystem atomic renames never show a fake copy bar. Output uses no
color, so `NO_COLOR` is naturally respected.

## Shell navigation helper

```bash
pcd() {
    local target
    target="$(proj path "$@")" || return
    cd "$target"
}
```

A child process cannot change its parent shell directory. After relocating a
project while the shell is inside it, `proj` prints the new `cd` command.

## Data-safety model

- `add` never moves data; `remove`, `forget`, and `unregister` never delete it.
- Import/archive/restore share one journaled relocation engine and never
  overwrite destinations.
- Canonical roots are real directories. Internal symlinks are copied as links
  and never followed; sockets, FIFOs, and device nodes cause refusal.
- Top-level `.git` files and repositories owning linked worktrees are refused.
- Cross-filesystem moves preflight size/free space, stage on the destination,
  snapshot and recheck source metadata, finalize atomically, commit the
  registry, recheck source device/inode, and only then remove the old source.
- If change, identity, commit, or cleanup is uncertain, preserving both copies
  wins over deleting data. Doctor reports incomplete journals but never chooses
  or deletes a copy.
- Generated-view failure never rolls back valid project data; use
  `proj reindex` after fixing the reported issue.

## Detection and deferred scope

Detection supports substantive Java, Kotlin, C, C++, C#, Go, Rust, Python,
JavaScript, TypeScript, Lua, shell, HTML, CSS, SQL, and assembly source, plus
Maven, Gradle, Cargo, Go Modules, npm, pnpm, Yarn, CMake, and MSBuild. It skips
common generated/vendor/cache directories and never follows symlinks. Data and
documentation formats are not classified as languages merely by existing.

V1 intentionally has no project deletion command, category or primary tag,
Git worktree migration, shell completion, daemon, watcher, synchronization,
IDE launcher, project generator, Git hosting automation, or web UI. Hard-link
topology, ACL/xattr preservation, sparse-copy optimization, and full-file
hashing are also deferred.

## Development checks

All tests use temporary homes, stores, mount topology, config, registry, and
state. They never access the real workspace or archive.

```bash
gofmt -w *.go
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/proj .
```
