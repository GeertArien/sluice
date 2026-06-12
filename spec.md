# Sluice — Build Specification

> A self-hosted web app that lets AI coding agents work on filtered copies of
> private repositories, and safely moves their work back upstream.
>
> *A sluice is the gate between two water levels: things pass through in both
> directions, but only under control. Same idea, but for git history.*

This document is a complete specification intended for a coding agent.
Section 12 contains verified git command sequences that MUST be treated as
the reference implementation for all git operations — the product is a web
UI and job orchestrator around exactly those operations.

---

## 1. Problem statement

An organization has one or more repositories containing folders that must
never be exposed to cloud-based AI models. AI agents (running in VMs) should
still be able to work on the rest of the codebase using a normal git forge
workflow (issues, branches, pull requests) against a private Gitea instance.

The solution: for each repository, maintain a **filtered mirror** on Gitea
where the excluded folders are removed from the *entire history* (worktree,
history, and forge API — `git filter-repo`, not sparse checkout). Agents
work only against the mirror. When an agent's PR is approved, the operator
**promotes** it: the app translates the filtered-history commits back onto
the real history and pushes a branch to the source remote, where the human
opens/merges the real PR. After the upstream merge, the app **finalizes**:
closes the Gitea PR, deletes branches, and re-syncs the mirror.

Today this exists as a set of bash scripts handling ONE repository
(included in Section 12). Sluice generalizes it to many repositories with a
web UI, scheduling, webhooks, job logs, and an audit trail.

## 2. Core domain concepts

- **Bridge** — the central entity. One bridge = one (source repo → Gitea
  repo) pair with its filter configuration. The app manages many bridges.
- **Sync** — update bridge mirror from source: fetch source, run
  deterministic `git filter-repo`, force-push configured branches to Gitea,
  store the commit-map, then run finalization checks.
- **Promotion** — translate an agent branch from the Gitea mirror into a
  branch on the source remote (patch-based, with security guard and
  optional identity rewrite).
- **Finalization** — detect that a promoted change landed upstream; close
  the Gitea PR, delete the Gitea branch and the upstream `ai/*` branch.
- **Commit-map** — per-bridge mapping `real-sha → filtered-sha` produced by
  filter-repo on every sync; required to resolve promotion bases.
- **Job** — any sync/promotion/finalization run executes as a background
  job with captured logs, status, and an audit record.

## 3. Suggested stack (substitute equivalents if justified)

- **Backend:** Go (single static binary) or Node/TypeScript. All git work
  shells out to the `git` CLI and `git-filter-repo`; do NOT reimplement git
  operations with a library — fidelity to Section 12 matters more than
  purity. Validate at startup that `git >= 2.32` and `git-filter-repo` are
  on PATH.
- **Storage:** SQLite (single file) for all state; filesystem workspace
  directory per bridge for mirrors/clones/commit-maps.
- **UI:** server-rendered pages + light interactivity (HTMX/Alpine or a
  small React app). No design-system bloat; function over form, but make
  status legible at a glance (see §6).
- **Deployment:** one Docker container; volume for SQLite + workspaces;
  outbound network access to source remotes and Gitea. No inbound access
  needed except the UI and webhook endpoint.
- **Auth:** single admin account (env-configured password or OIDC later).
  Session cookie. Webhook endpoint authenticated by per-bridge secret.

## 4. Data model (minimum)

```
bridges
  id, name, slug
  source_remote_url        (ssh or https)
  gitea_base_url, gitea_owner, gitea_repo, gitea_ssh_url
  gitea_token              (encrypted at rest, see §9)
  excluded_paths           (JSON array of plain relative dir paths)
  sync_branches            (JSON array, e.g. ["master"])
  sync_globs               (JSON array, e.g. ["refs/heads/release/*"])
  promote_name, promote_email, promote_keep_trailer   (nullable)
  promote_signoff          (bool)
  schedule_cron            (nullable)
  webhook_secret
  status                   (active | paused | error)
  created_at, updated_at

jobs
  id, bridge_id, kind (sync|promote|finalize|init|verify), status
  (queued|running|success|failed|needs_attention), started_at, finished_at,
  log (text, append-only), error_summary, payload (JSON: e.g. branch name)

promotions
  id, bridge_id, gitea_branch, gitea_pr_number (nullable),
  real_branch, real_tip_sha, base_branch, status
  (promoted|landed|finalized|conflict|rejected), created_at, finalized_at

audit_log
  id, bridge_id, actor (admin|scheduler|webhook), action, details(JSON), at
```

Workspace layout per bridge (on disk, path derived from slug):

```
<workdir>/<slug>/source-mirror/   bare mirror of source (filtering input)
<workdir>/<slug>/source-work/     working clone of source (promotion target)
<workdir>/<slug>/gitea-clone/     clone of the Gitea mirror (promotion source)
<workdir>/<slug>/commit-map       latest map from the most recent sync
```

## 5. Functional requirements

### 5.1 Bridge management
- CRUD for bridges with validation: test source fetch, test Gitea API token,
  test Gitea SSH push (to a temp ref), reject excluded paths containing
  regex metacharacters, whitespace edges, leading `/`, or `..`.
- **Init flow** (wizard): create the Gitea repo via API if missing
  (private; handle org vs user owner), clone mirror + work clone, run first
  sync, then run **verification** (5.2) and show the result before the
  bridge can be set `active`.
- Editing `excluded_paths` on an existing bridge triggers a warning modal:
  changing the filter changes ALL filtered SHAs (determinism only holds for
  an unchanged filter); existing agent branches on Gitea become orphaned.
  Require typed confirmation; on confirm, run a full re-sync and mark all
  open promotions `needs_attention`.

### 5.2 Verification (leak check)
After init and on demand: in a temp clone of the filtered repo run
- `git log --all -- <each excluded path>` → must be empty;
- optionally run `gitleaks detect` if the binary is present, and surface
  findings (advisory, not blocking);
- a configurable list of "tripwire strings" per bridge (e.g. a secret
  project codename) grepped across all blobs:
  `git grep -I <s> $(git rev-list --all)` → must be empty.
Result stored on the bridge and shown on the dashboard ("last verified").

### 5.3 Sync
- Triggers: manual button, per-bridge cron schedule, webhook
  (`POST /hooks/<bridge-slug>` with shared secret; coalesce bursts —
  debounce ~30s).
- Executes the Section 12.1 sequence as a job. On success, stores the new
  commit-map and runs finalization (5.5).
- Per-bridge mutex: sync, promote, and finalize for the same bridge never
  run concurrently (queue them). Different bridges run in parallel
  (bounded worker pool).

### 5.4 Promotion
- UI lists open Gitea PRs per bridge (live from the Gitea API), each with:
  head branch, base, title, author, mergeable status, and pre-flight
  checks computed on demand:
  - linearity (`git rev-list --merges --count base..tip` == 0),
  - commit-map resolution of the merge-base,
  - excluded-path guard (12.2 step 4) — show PASS/FAIL prominently,
  - ahead/behind vs mirror base ("stale branch" warning).
- "Promote" button → job running Section 12.2. Show a **preview** first:
  list of commits (message, author, files touched) and, if identity
  rewrite is configured, the resulting author/committer/trailer.
- On `git am` conflict: job ends in `needs_attention`; the UI shows the
  exact recovery commands (resolve in `source-work`, `git am --continue`,
  push, then a "mark promoted manually" form that records the tip SHA).
  Also offer a one-click "abort" (`git am --abort`, reset workspace).
- On success: record the promotion, comment on the Gitea PR via API
  ("Promoted upstream as `ai/<branch>` …"), and display the upstream
  branch name + a copyable compare URL if the source host is recognizable
  (GitHub/GitLab/Gitea URL patterns).

### 5.5 Finalization
- After every sync, and on demand: for each promotion in `promoted` state
  run the Section 12.3 detection (ancestor check, then `git cherry`
  patch-id equivalence against `origin/<base>`).
- Landed → close Gitea PR with a comment, delete the Gitea branch, delete
  the upstream `ai/*` branch, set status `finalized`.
- Squash merges upstream are NOT auto-detectable (patch-ids change):
  provide a per-promotion "Mark as merged" button (manual finalize).

### 5.6 Dashboard & observability
- Home: one row/card per bridge — status, last sync time + result, last
  verification, # open agent PRs, # promotions awaiting upstream merge,
  # jobs needing attention. Anything red must be visible without clicking.
- Bridge detail: tabs for Overview, Agent PRs, Promotions, Jobs, Settings.
- Job detail: live-streaming log (poll or SSE), duration, retry button
  where safe (sync: yes; promote: only after abort).
- Audit log view, filterable by bridge/action/date.

## 6. UI tone
Operator tool, not a marketing site. Dense tables, monospace for SHAs/
branches (truncate SHAs to 12 chars, click to copy), explicit empty states,
every destructive or upstream-affecting action behind a confirm dialog that
restates exactly what will happen ("This will force-push 3 branches to
gitea.local/ai/monorepo").

## 7. Background jobs
- Simple DB-backed queue (no external broker). Worker pool size
  configurable. Jobs are resumable-safe: any crash mid-job leaves the
  workspace recoverable — every job starts by sanity-checking/cleaning its
  workspace (e.g. `git am --abort` if a stale `rebase-apply` dir exists,
  remove stale temp dirs).
- All shell-outs run with timeouts, captured stdout/stderr appended to the
  job log, and never with `shell=true` string interpolation — argv arrays
  only (paths and branch names are untrusted input; see §9).

## 8. HTTP surface (sketch)

```
GET  /                         dashboard
GET  /bridges/:slug            bridge detail
POST /bridges                  create (wizard steps may be separate calls)
PUT  /bridges/:slug            update settings
POST /bridges/:slug/sync       enqueue sync
POST /bridges/:slug/verify     enqueue verification
POST /bridges/:slug/promote    {branch, base} → enqueue promotion
POST /promotions/:id/finalize  manual "mark as merged"
POST /promotions/:id/abort     abort a conflicted promotion
GET  /jobs/:id                 job detail (+ /jobs/:id/log streaming)
POST /hooks/:slug              webhook (secret-authenticated), enqueue sync
```

## 9. Security requirements (treat as hard requirements)

1. **The excluded-path guard is the security boundary on the return path.**
   It must run on the generated patch files (not on a diff recomputed some
   other way), must match both `+++ b/<path>` and `--- a/<path>` prefixes
   with path-boundary anchoring (`(/|$)` after the path), and a guard
   failure must hard-fail the promotion, mark it `rejected`, write an audit
   entry, and surface a red banner. There is no override button.
2. Never push any ref from an unfiltered repository to Gitea. The only
   writer to Gitea git refs is the sync job operating on the
   filter-repo output. Enforce structurally (the code path that touches
   `source-mirror`/`source-work` has no Gitea push capability).
3. Secrets: Gitea tokens and webhook secrets encrypted at rest (e.g.
   age/NaCl secretbox with a key from env); never written to job logs —
   scrub `Authorization` headers and tokens from any logged command/output.
4. SSH host keys for source and Gitea pinned via a managed
   `known_hosts`; no `StrictHostKeyChecking=no`.
5. All git/filter-repo invocations: argv arrays, explicit `--` separators
   where git accepts them, branch names validated with
   `git check-ref-format --branch` before use.
6. CSRF protection on all POSTs; webhook bodies verified against the
   bridge secret (constant-time compare).
7. The app itself should be deployable on a host the agents cannot reach.

## 10. Edge cases the implementation MUST handle

- **Determinism contract:** re-running filter-repo with an unchanged filter
  on extended history yields identical SHAs for old commits → force-push is
  safe and agent branches stay valid. Changing the filter breaks this
  (handled via 5.1's confirm-and-resync flow).
- Gitea PRs are **closed, not merged**, at finalization — synced-back
  commits have different SHAs/committers. The PR comment must explain this
  so agents/humans don't read it as rejection.
- Promotion of a branch whose Gitea PR doesn't exist (branch-only) — allow
  it; `gitea_pr_number` stays null and finalization skips the PR steps.
- Merge commits in an agent branch → reject with the exact remediation
  message ("rebase onto <base>, force-push, retry").
- Empty promotions (branch even with base) → reject cleanly.
- Binary files in patches → supported (`format-patch --binary`).
- Identity rewrite (12.2 step 7) preserves author dates and adds
  `Co-authored-by` trailers; skip the trailer when the original author
  already equals the configured identity. Optional `--signoff` on `git am`.
- finalize detection: ancestor check first (merge/ff), then `git cherry`
  (rebase-merge); squash merge → manual button only.
- Commit-map lookups that fail (agent branched before the bridge existed,
  or map lost) → fail with "run sync first" guidance, never guess.
- Large repos: first sync/filter can take minutes; jobs must stream
  progress and the UI must not block.
- Source remote moved/force-pushed history → sync detects non-FF in the
  mirror (`remote update` handles it for a `--mirror` clone), but flag
  upstream history rewrites in the job log as a warning since they
  invalidate determinism for rewritten ranges.

## 11. Phases

- **MVP:** bridges CRUD + init wizard, sync (manual/cron/webhook),
  verification, promotion with guard + identity rewrite + conflict
  handling, finalization (auto + manual), dashboard, jobs+logs, audit,
  single admin auth.
- **Phase 2:** auto-create the upstream PR via the source host's API
  (GitHub/GitLab/Gitea drivers); per-agent statistics; multi-user/roles;
  tripwire-string library shared across bridges.
- **Phase 3 (optional):** alternative "Josh driver" — replace the
  patch-based promotion core with a push through a Josh proxy, keeping all
  UI/state/finalization identical. Design the promotion step behind an
  interface from day one to keep this swap clean.

## 12. Reference implementation (verified git sequences)

These sequences were tested end-to-end and are normative. `$EXCLUDED[@]`
are plain directory paths; `$BASE` is the target branch (e.g. `master`).

### 12.1 Sync (source → Gitea)

```bash
git -C source-mirror remote update --prune          # mirror created with: git clone --mirror $SOURCE_REMOTE

tmp=$(mktemp -d)
git clone --no-local source-mirror "$tmp/filtered"
cd "$tmp/filtered"
args=(); for p in "${EXCLUDED[@]}"; do args+=(--path "$p"); done
git filter-repo --invert-paths "${args[@]}"          # deterministic; converts remote refs to local heads
cp .git/filter-repo/commit-map <workspace>/commit-map   # lines: "<real-sha> <filtered-sha>"

git remote add gitea "$GITEA_SSH"
for b in "${SYNC_BRANCHES[@]}"; do git push --force gitea "refs/heads/$b:refs/heads/$b"; done
for g in "${SYNC_GLOBS[@]}";    do git push --force gitea "$g:$g" || true; done
```

### 12.2 Promotion (Gitea branch → source remote)

```bash
# 1. resolve
git -C gitea-clone fetch --prune origin
TIP=$(git -C gitea-clone rev-parse "origin/$BRANCH")
BASE_FILTERED=$(git -C gitea-clone merge-base "origin/$BASE" "$TIP")
BASE_REAL=$(awk -v f="$BASE_FILTERED" '$2==f{print $1; exit}' commit-map)   # empty → fail "run sync"

# 2. linearity
[ "$(git -C gitea-clone rev-list --merges --count "$BASE_FILTERED..$TIP")" -eq 0 ] || fail

# 3. export
git -C gitea-clone format-patch --binary -o "$PATCH_DIR" "$BASE_FILTERED..$TIP"

# 4. SECURITY GUARD (hard fail, no override)
pattern=$(IFS='|'; echo "${EXCLUDED[*]}")
grep -lE "^(\+\+\+ b|--- a)/($pattern)(/|$)" "$PATCH_DIR"/*.patch && fail "touches excluded paths"

# 5. apply onto real history
cd source-work && git fetch origin
git checkout -B "ai/$BRANCH" "$BASE_REAL"
git -c user.name="$COMMITTER_NAME" -c user.email="$COMMITTER_EMAIL" am --3way "$PATCH_DIR"/*.patch
#   [--signoff if configured]  conflict → status needs_attention, offer --continue/--abort paths

# 6. optional identity rewrite (author AND committer; keeps author dates)
#    run per commit via: git rebase --exec <script> "$BASE_REAL"
#    script body:
#      orig_name=$(git log -1 --format='%an'); orig_email=$(git log -1 --format='%ae')
#      orig_date=$(git log -1 --format='%aD')
#      args=(--amend --no-edit --author="$PROMOTE_NAME <$PROMOTE_EMAIL>" --date="$orig_date")
#      [ "$PROMOTE_KEEP_TRAILER" = true ] && [ "$orig_email" != "$PROMOTE_EMAIL" ] \
#        && args+=(--trailer "Co-authored-by: $orig_name <$orig_email>")
#      git -c user.name="$PROMOTE_NAME" -c user.email="$PROMOTE_EMAIL" commit "${args[@]}"

# 7. push + record
git push -u origin "ai/$BRANCH"
# record: gitea branch, PR number (Gitea API: GET /repos/{o}/{r}/pulls?state=open, match head.ref),
#         real branch, $(git rev-parse HEAD), base
# comment on the Gitea PR (POST /repos/{o}/{r}/issues/{n}/comments)
```

### 12.3 Finalization detection

```bash
git -C source-work fetch --prune origin
# landed if:
git merge-base --is-ancestor "$REAL_TIP" "origin/$BASE"        # merge or fast-forward
# else landed if no '+' lines from:
git cherry "origin/$BASE" "$REAL_TIP"                          # rebase-merge (patch-id equivalence)
# squash merge: manual only.
# cleanup: PATCH Gitea PR {"state":"closed"} + comment; DELETE Gitea branch;
#          git push origin --delete "ai/$BRANCH"
```

### 12.4 Gitea API calls used

```
POST   /api/v1/orgs/{owner}/repos        create repo (or POST /api/v1/user/repos for user owner)
GET    /api/v1/repos/{o}/{r}/pulls?state=open[&limit=50]
PATCH  /api/v1/repos/{o}/{r}/pulls/{index}        {"state":"closed"}
POST   /api/v1/repos/{o}/{r}/issues/{index}/comments   {"body": "..."}
DELETE /api/v1/repos/{o}/{r}/branches/{branch}
Auth: header "Authorization: token <token>"
```

## 13. Acceptance tests (encode as e2e tests against throwaway repos)

1. **Filter correctness:** create a repo with public + excluded paths and
   commits touching only excluded paths → after sync, the Gitea mirror has
   no excluded files in any commit and the excluded-only commits are gone.
2. **Determinism:** add upstream commits, sync again → previously filtered
   SHAs unchanged; an agent branch created before the second sync still has
   a resolvable merge-base.
3. **Round-trip:** agent commit on the mirror → promote → commit applies on
   real history with excluded files intact beside it; author preserved (no
   rewrite configured).
4. **Identity rewrite:** with PROMOTE_* set → upstream commits have the
   configured author+committer, original author dates, and per-agent
   `Co-authored-by` trailers.
5. **Guard:** agent patch creating a file under an excluded path → promotion
   `rejected`, nothing pushed upstream, audit entry written.
6. **Merge-commit rejection**, **empty-branch rejection**, **am-conflict →
   needs_attention → abort** flows.
7. **Finalize:** merge upstream (true merge) → next sync auto-closes the
   Gitea PR and deletes both branches; squash merge → only manual finalize
   works.
8. **Concurrency:** sync and promote enqueued together for one bridge run
   serially; two bridges run in parallel.
9. **Secret hygiene:** job logs contain no token material.
```
