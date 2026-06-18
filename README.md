# Sluice

A self-hosted web app that lets AI coding agents work on **filtered copies**
of private repositories, and safely moves their work back upstream.

> *A sluice is the gate between two water levels: things pass through in both
> directions, but only under control. Same idea, but for git history.*

For each repository, Sluice maintains a **filtered mirror** on a private
Gitea instance where excluded folders are removed from the *entire history*
(`git filter-repo`, not sparse checkout). Agents work only against the
mirror via the normal forge workflow. When an agent's PR is approved, the
operator **promotes** it: Sluice translates the filtered-history commits
back onto the real history and pushes a branch to the source remote (named
after the agent branch by default, editable in the pre-flight screen). After
the upstream merge, Sluice **finalizes**: closes the Gitea PR
and deletes both branches.

The full specification lives in [spec.md](spec.md). The git command
sequences in spec §12 are the normative reference implementation; Sluice is
a web UI and job orchestrator around exactly those operations.

## Quick start

```sh
docker build -t sluice .
docker run -d --name sluice \
  -p 8080:8080 \
  -v sluice-data:/data \
  -e SLUICE_ADMIN_PASSWORD='change-me' \
  -e SLUICE_SECRET_KEY="$(openssl rand -hex 32)" \
  sluice
```

Then open http://localhost:8080, sign in, and create your first bridge.
The init wizard creates the Gitea repo (private) if missing, clones the
source, runs the first filtered sync, and runs the **leak-check
verification** — the bridge stays paused until you review the result and
activate it.

SSH remotes need two things: a key, and trusted host keys (Sluice pins host
keys and never uses `StrictHostKeyChecking=no`).

**Host keys — Trusted hosts page.** Sluice manages `known_hosts` for you at
`$SLUICE_DATA_DIR/known_hosts` (on the data volume — don't mount it read-only).
On the **Trusted hosts** page, scan your source and Gitea hosts, verify the
SHA256 fingerprints against what the provider publishes, and trust them. Any
entries already in the file are imported under management on startup.

**Key — SSH keys page (recommended).** Generate a named ed25519 keypair (or
paste an existing one). Sluice stores the private key encrypted at rest and
shows the public key to register as a **write-enabled** deploy key on the
source and the Gitea mirror. Each bridge selects a named key from a dropdown,
so one key can be reused across bridges (or use a separate key per source).

**Mounted key (fallback).** If a bridge selects no managed key, ssh uses the
container's default identity, so you can instead mount one key for all bridges:

```sh
  -v $PWD/id_ed25519:/home/sluice/.ssh/id_ed25519:ro
```

The source key needs **push** access (promotion pushes the promoted branch and
finalize deletes it), and the same identity is used for the Gitea push.

### Running on a bind-mounted volume (unraid / NAS)

The container starts as root, takes ownership of the data directory, then
drops to an unprivileged user. On a host where the data folder is owned by a
specific account (unraid's appdata share is `nobody:users` = `99:100`), set
`PUID`/`PGID` to that account so the files Sluice writes are owned correctly:

```sh
  -e PUID=99 -e PGID=100 \
  -v /mnt/user/appdata/sluice:/data
```

`PUID`/`PGID` default to `1000:1000`. If you instead pin the container user
yourself (`docker run --user 99:100`), the entrypoint skips the remap and you
are responsible for the mounted data dir being writable by that user.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `SLUICE_ADMIN_PASSWORD` | *(required)* | single admin login |
| `SLUICE_SECRET_KEY` | *(required)* | 64 hex chars; encrypts Gitea tokens & webhook secrets at rest (NaCl secretbox) |
| `SLUICE_DATA_DIR` | `data` | SQLite DB + per-bridge workspaces |
| `SLUICE_LISTEN` | `:8080` | listen address |
| `SLUICE_KNOWN_HOSTS` | `$SLUICE_DATA_DIR/known_hosts` | managed pinned SSH host keys (edited via the Trusted hosts page; must be writable) |
| `SLUICE_WORKERS` | `4` | job worker pool size |
| `PUID` / `PGID` | `1000` / `1000` | user/group the app runs as; set to the owner of the mounted data dir (unraid: `99`/`100`) |

Sluice validates at startup that `git >= 2.32` and `git-filter-repo` are on
`PATH`. If `gitleaks` is installed it runs during verification as an
advisory scan.

## Core concepts

- **Bridge** — one (source repo → Gitea repo) pair with its filter config.
- **Sync** — fetch source, deterministic `git filter-repo`, force-push the
  configured branches to Gitea, store the commit-map, run finalization
  checks. Triggered manually, by cron, or by webhook
  (`POST /hooks/<slug>` with `X-Sluice-Secret`; bursts debounced ~30s).
  See [docs/syncing.md](docs/syncing.md) for the full flow with diagrams.
- **Promotion** — translate an agent branch from the mirror onto the source
  remote, pushed under the agent branch name by default (editable per
  promotion in the pre-flight screen) — patch-based: `format-patch` →
  security guard → `git am --3way`, optional identity rewrite with
  preserved author dates and `Co-authored-by` trailers).
- **Finalization** — detect that a promoted change landed upstream
  (ancestor check, then `git cherry` patch-id equivalence), close the Gitea
  PR with an explanatory comment, delete both branches. Squash merges are
  not auto-detectable — use the per-promotion **Mark as merged** button.

## Security model

1. **The excluded-path guard is the boundary on the return path.** It runs
   on the exact patch files that will be applied, matches `+++ b/<path>`
   and `--- a/<path>` with path-boundary anchoring, and a failure
   hard-fails the promotion (status `rejected`, audit entry, red banner).
   There is no override.
2. Only the sync job pushes to Gitea, and only from filter-repo output —
   the promotion code path has no Gitea push capability by construction.
3. Gitea tokens and webhook secrets are encrypted at rest; secret material
   is scrubbed from job logs.
4. All git invocations are argv arrays (no shell interpolation); branch
   names are validated with `git check-ref-format` before use.
5. CSRF protection on all UI POSTs; webhooks verified with a constant-time
   compare; SSH host keys pinned via a managed `known_hosts`.
6. Per-bridge mutex: sync/promote/finalize for one bridge never run
   concurrently; different bridges run in parallel.

**Caveat:** changing a bridge's excluded paths rewrites *all* filtered SHAs
(determinism only holds for an unchanged filter). The settings page
requires typed confirmation, then re-syncs and flags open promotions.

## Development

```sh
go build ./...
go test ./...     # includes e2e acceptance tests against throwaway repos
```

The e2e tests (`internal/engine`, `internal/jobs`) require `git` and
`git-filter-repo` locally; they skip when filter-repo is missing.

### Layout

```
cmd/sluice/          entrypoint, startup validation
internal/execx/      argv-only command runner: timeouts, log capture, secret scrubbing
internal/engine/     spec §12 git sequences: sync, promote, finalize, verify, guard
internal/jobs/       DB-backed queue, workers, cron, webhook debounce, Gitea side effects
internal/gitea/      minimal Gitea API client (spec §12.4)
internal/store/      SQLite: bridges, jobs, promotions, audit log
internal/secrets/    secretbox encryption for tokens at rest
internal/web/        server-rendered UI, auth, CSRF, webhook endpoint
```

## Roadmap (spec §11)

- **Phase 2:** auto-create the upstream PR via the source host's API,
  per-agent statistics, multi-user/roles, shared tripwire-string library.
- **Phase 3:** alternative Josh-proxy promotion driver. The promotion core
  already sits behind the engine boundary to keep this swap clean.
