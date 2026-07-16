package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geertarien/sluice/internal/execx"
)

// The tests in this file encode the spec §13 acceptance tests against
// throwaway local repositories: a bare "source" remote, a bare repo
// standing in for the Gitea mirror (git pushes to a filesystem path the
// same way it pushes over SSH), and a scratch clone playing the agent.

func requireFilterRepo(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "filter-repo", "--version").Run(); err != nil {
		t.Skip("git-filter-repo not installed")
	}
}

type fixture struct {
	t       *testing.T
	dir     string
	src     string // bare source remote
	gitea   string // bare filtered mirror ("Gitea")
	dev     string // human clone of source
	workdir string
	eng     *Engine
	bridge  *Bridge
	logs    *strings.Builder
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireFilterRepo(t)
	dir := t.TempDir()
	f := &fixture{
		t:       t,
		dir:     dir,
		src:     filepath.Join(dir, "src.git"),
		gitea:   filepath.Join(dir, "gitea.git"),
		dev:     filepath.Join(dir, "dev"),
		workdir: filepath.Join(dir, "work"),
		logs:    &strings.Builder{},
	}
	f.git(dir, "init", "--bare", "-b", "main", f.src)
	f.git(dir, "init", "--bare", "-b", "main", f.gitea)
	f.git(dir, "clone", f.src, f.dev)
	f.git(f.dev, "config", "user.name", "Upstream Dev")
	f.git(f.dev, "config", "user.email", "dev@example.com")

	// Seed history: public files plus an excluded folder with a secret.
	f.commit(f.dev, "public/app.txt", "line one\nline two\n", "add app")
	f.commit(f.dev, "secret/key.txt", "PROJECT-CODENAME-X\n", "add secret")
	f.commit(f.dev, "public/readme.md", "# readme\n", "add readme")
	f.git(f.dev, "push", "origin", "main")

	if err := os.MkdirAll(f.workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &execx.Runner{Log: func(s string) { f.logs.WriteString(s + "\n") }}
	f.eng = New(f.workdir, "", "", runner)
	f.bridge = &Bridge{
		Slug:            "test",
		SourceRemoteURL: f.src,
		GiteaSSHURL:     f.gitea,
		ExcludedPaths:   []string{"secret"},
		SyncBranches:    []string{"main"},
	}
	return f
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitErr runs git allowing failure; returns output and error.
func (f *fixture) gitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *fixture) commit(repo, file, content, msg string) {
	f.t.Helper()
	full := filepath.Join(repo, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(repo, "add", "--", file)
	f.git(repo, "commit", "-m", msg)
}

func (f *fixture) initAndSync() {
	f.t.Helper()
	ctx := context.Background()
	if err := f.eng.InitWorkspace(ctx, f.bridge); err != nil {
		f.t.Fatalf("init: %v\nlog:\n%s", err, f.logs)
	}
	if err := f.eng.Sync(ctx, f.bridge); err != nil {
		f.t.Fatalf("sync: %v\nlog:\n%s", err, f.logs)
	}
}

// agentClone clones the mirror as the agent would and returns its path.
func (f *fixture) agentClone(name string) string {
	f.t.Helper()
	dir := filepath.Join(f.dir, name)
	f.git(f.dir, "clone", f.gitea, dir)
	f.git(dir, "config", "user.name", "Agent Smith")
	f.git(dir, "config", "user.email", "agent@vm.local")
	return dir
}

// --- §13.1 filter correctness ---

func TestSyncFiltersExcludedPathsFromAllHistory(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	check := filepath.Join(f.dir, "check")
	f.git(f.dir, "clone", f.gitea, check)
	if out := f.git(check, "log", "--all", "--oneline", "--", "secret"); out != "" {
		t.Fatalf("excluded path still in filtered history:\n%s", out)
	}
	// The excluded-only commit ("add secret") must be gone entirely.
	if out := f.git(check, "log", "--format=%s", "main"); strings.Contains(out, "add secret") {
		t.Fatalf("excluded-only commit survived filtering:\n%s", out)
	}
	// Tripwire content must not exist in any blob.
	res, err := f.eng.Verify(context.Background(), &Bridge{
		Slug: "test", GiteaSSHURL: f.gitea,
		ExcludedPaths: []string{"secret"}, TripwireStrings: []string{"PROJECT-CODENAME-X"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("verify should pass on clean mirror: %+v", res)
	}
}

func TestVerifyCatchesTripwireString(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()
	// "line one" exists in public content, so this tripwire must fire.
	res, err := f.eng.Verify(context.Background(), &Bridge{
		Slug: "test", GiteaSSHURL: f.gitea,
		ExcludedPaths: []string{"secret"}, TripwireStrings: []string{"line one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.TripwireFindings) != 1 {
		t.Fatalf("tripwire not detected: %+v", res)
	}
}

// --- §13.2 determinism ---

func TestSyncIsDeterministicAcrossExtendedHistory(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()
	firstTip := f.git(f.gitea, "rev-parse", "main")

	// Agent branches off the mirror before the second sync.
	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/feature.txt", "agent work\n", "agent: add feature")
	f.git(agent, "push", "origin", "feature")

	// Upstream grows; sync again.
	f.commit(f.dev, "public/more.txt", "more\n", "upstream more")
	f.git(f.dev, "push", "origin", "main")
	if err := f.eng.Sync(context.Background(), f.bridge); err != nil {
		t.Fatalf("second sync: %v\nlog:\n%s", err, f.logs)
	}

	newTip := f.git(f.gitea, "rev-parse", "main")
	if newTip == firstTip {
		t.Fatal("mirror did not advance")
	}
	if parent := f.git(f.gitea, "rev-parse", "main~1"); parent != firstTip {
		t.Fatalf("previously filtered SHA changed: old tip %s, new tip's parent %s", firstTip, parent)
	}
	// The agent branch must still have a resolvable merge-base in the map.
	pf, err := f.eng.RunPreflight(context.Background(), f.bridge, "feature", "main")
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if pf.BaseReal == "" || pf.BaseRealErr != "" {
		t.Fatalf("merge-base not resolvable after second sync: %+v", pf)
	}
}

// --- promotion-ignored paths ---

func TestPromoteStripsIgnoredPaths(t *testing.T) {
	f := newFixture(t)
	f.bridge.PromoteIgnorePaths = []string{"tooling"}
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "work")
	// A mirror-only build helper (squash-merged in, so no merge commit) ...
	f.commit(agent, "tooling/prebuilt.bin", "LIB\n", "add build helper")
	// ... plus the agent's real change.
	f.commit(agent, "public/feature.txt", "real work\n", "agent: feature")
	f.git(agent, "push", "origin", "work")

	res, err := f.eng.Promote(context.Background(), f.bridge, "work", "main", "")
	if err != nil {
		t.Fatalf("promote: %v\nlog:\n%s", err, f.logs)
	}
	files := f.git(f.src, "ls-tree", "-r", "--name-only", res.RealBranch)
	if !strings.Contains(files, "public/feature.txt") {
		t.Fatalf("real change missing upstream:\n%s", files)
	}
	if strings.Contains(files, "tooling/") {
		t.Fatalf("ignored path leaked to source:\n%s", files)
	}
}

func TestPromoteRejectsWhenOnlyIgnoredPathsChanged(t *testing.T) {
	f := newFixture(t)
	f.bridge.PromoteIgnorePaths = []string{"tooling"}
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "buildonly")
	f.commit(agent, "tooling/prebuilt.bin", "LIB\n", "add build helper")
	f.git(agent, "push", "origin", "buildonly")

	_, err := f.eng.Promote(context.Background(), f.bridge, "buildonly", "main", "")
	var rej *ErrRejected
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "promotion-ignored") {
		t.Fatalf("expected rejection (all changes ignored), got %v", err)
	}
	if _, err := f.gitErr(f.src, "rev-parse", "--verify", "buildonly"); err == nil {
		t.Fatal("nothing should have been pushed upstream")
	}
}

// TestPreflightWithIgnoredPaths exercises the pre-flight screen when the bridge
// has promotion-ignored paths. CommitCount is computed via a rev-list that
// carries the "-- . :(exclude)…" pathspec, so a mis-ordered range would make
// git exit 129 here (the reported bug). The count must reflect the commits that
// still have changes after the ignored paths are stripped.
func TestPreflightWithIgnoredPaths(t *testing.T) {
	f := newFixture(t)
	f.bridge.PromoteIgnorePaths = []string{"tooling"}
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "work")
	// A mirror-only helper commit (dropped from the promotion) ...
	f.commit(agent, "tooling/prebuilt.bin", "LIB\n", "add build helper")
	// ... and two real changes that survive the exclude.
	f.commit(agent, "public/a.txt", "a\n", "agent: change a")
	f.commit(agent, "public/b.txt", "b\n", "agent: change b")
	f.git(agent, "push", "origin", "work")

	pf, err := f.eng.RunPreflight(context.Background(), f.bridge, "work", "main")
	if err != nil {
		t.Fatalf("preflight: %v\nlog:\n%s", err, f.logs)
	}
	if pf.CommitCount != 2 {
		t.Fatalf("CommitCount = %d, want 2 (ignored-only commit excluded)", pf.CommitCount)
	}
	if pf.MergeCount != 0 {
		t.Fatalf("MergeCount = %d, want 0", pf.MergeCount)
	}
	if !pf.GuardOK {
		t.Fatalf("guard should pass, got detail: %s", pf.GuardDetail)
	}
	// The commit preview must exclude the ignored-only commit as well.
	for _, c := range pf.Commits {
		if strings.Contains(c.Subject, "build helper") {
			t.Fatalf("ignored-only commit leaked into the preview: %+v", pf.Commits)
		}
	}
}

func TestRoundTripPromotionPreservesAuthorAndExcludedFiles(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/feature.txt", "agent work\n", "agent: add feature")
	f.git(agent, "push", "origin", "feature")

	res, err := f.eng.Promote(context.Background(), f.bridge, "feature", "main", "")
	if err != nil {
		t.Fatalf("promote: %v\nlog:\n%s", err, f.logs)
	}
	if res.RealBranch != "feature" || res.NumCommits != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Upstream branch exists with the agent's author and the secret intact.
	author := f.git(f.src, "log", "-1", "--format=%an <%ae>", "feature")
	if author != "Agent Smith <agent@vm.local>" {
		t.Fatalf("author not preserved: %q", author)
	}
	files := f.git(f.src, "ls-tree", "-r", "--name-only", "feature")
	if !strings.Contains(files, "secret/key.txt") || !strings.Contains(files, "public/feature.txt") {
		t.Fatalf("promoted branch missing files:\n%s", files)
	}
}

// --- promotion target branch name ---

func TestPromoteCustomTargetBranch(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "work")
	f.commit(agent, "public/x.txt", "x\n", "agent: work")
	f.git(agent, "push", "origin", "work")

	res, err := f.eng.Promote(context.Background(), f.bridge, "work", "main", "release/from-agent")
	if err != nil {
		t.Fatalf("promote: %v\nlog:\n%s", err, f.logs)
	}
	if res.RealBranch != "release/from-agent" {
		t.Fatalf("real branch = %q, want release/from-agent", res.RealBranch)
	}
	if _, err := f.gitErr(f.src, "rev-parse", "--verify", "refs/heads/release/from-agent"); err != nil {
		t.Fatal("custom target branch not pushed upstream")
	}
	// The agent's own branch name must not have been created upstream.
	if _, err := f.gitErr(f.src, "rev-parse", "--verify", "refs/heads/work"); err == nil {
		t.Fatal("agent branch name should not exist upstream when a custom target is given")
	}
}

func TestPromoteRejectsTargetEqualBase(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "work")
	f.commit(agent, "public/x.txt", "x\n", "agent: work")
	f.git(agent, "push", "origin", "work")

	_, err := f.eng.Promote(context.Background(), f.bridge, "work", "main", "main")
	var rej *ErrRejected
	if !errors.As(err, &rej) {
		t.Fatalf("expected rejection when target == base, got %v", err)
	}
	// The base branch upstream must be untouched (still reachable, not force-pushed).
	if _, err := f.gitErr(f.src, "rev-parse", "--verify", "refs/heads/main"); err != nil {
		t.Fatal("base branch should still exist")
	}
}

// --- §13.4 identity rewrite ---

func TestIdentityRewriteSetsAuthorCommitterAndTrailer(t *testing.T) {
	f := newFixture(t)
	f.bridge.PromoteName = "AI Bot"
	f.bridge.PromoteEmail = "ai-bot@corp.example"
	f.bridge.PromoteKeepTrailer = true
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/feature.txt", "agent work\n", "agent: add feature")
	origDate := f.git(agent, "log", "-1", "--format=%aI")
	f.git(agent, "push", "origin", "feature")

	if _, err := f.eng.Promote(context.Background(), f.bridge, "feature", "main", ""); err != nil {
		t.Fatalf("promote: %v\nlog:\n%s", err, f.logs)
	}
	show := f.git(f.src, "log", "-1", "--format=%an|%ae|%cn|%ce|%aI", "feature")
	parts := strings.Split(show, "|")
	if parts[0] != "AI Bot" || parts[1] != "ai-bot@corp.example" || parts[2] != "AI Bot" || parts[3] != "ai-bot@corp.example" {
		t.Fatalf("identity not rewritten: %s", show)
	}
	if parts[4] != origDate {
		t.Fatalf("author date not preserved: got %s want %s", parts[4], origDate)
	}
	body := f.git(f.src, "log", "-1", "--format=%B", "feature")
	if !strings.Contains(body, "Co-authored-by: Agent Smith <agent@vm.local>") {
		t.Fatalf("missing Co-authored-by trailer:\n%s", body)
	}
}

func TestIdentityRewriteSkipsTrailerForSameIdentity(t *testing.T) {
	f := newFixture(t)
	f.bridge.PromoteName = "Agent Smith"
	f.bridge.PromoteEmail = "agent@vm.local"
	f.bridge.PromoteKeepTrailer = true
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/feature.txt", "agent work\n", "agent: add feature")
	f.git(agent, "push", "origin", "feature")

	if _, err := f.eng.Promote(context.Background(), f.bridge, "feature", "main", ""); err != nil {
		t.Fatalf("promote: %v\nlog:\n%s", err, f.logs)
	}
	body := f.git(f.src, "log", "-1", "--format=%B", "feature")
	if strings.Contains(body, "Co-authored-by") {
		t.Fatalf("trailer added despite identical identity:\n%s", body)
	}
}

// --- §13.5 guard ---

func TestGuardRejectsPatchTouchingExcludedPath(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	// The agent recreates a file under the excluded path on the mirror.
	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "sneaky")
	f.commit(agent, "secret/exfil.txt", "planted\n", "agent: innocent-looking change")
	f.git(agent, "push", "origin", "sneaky")

	_, err := f.eng.Promote(context.Background(), f.bridge, "sneaky", "main", "")
	var guard *ErrGuardViolation
	if !errors.As(err, &guard) {
		t.Fatalf("expected guard violation, got %v", err)
	}
	// Nothing may have been pushed upstream.
	if _, err := f.gitErr(f.src, "rev-parse", "--verify", "sneaky"); err == nil {
		t.Fatal("guard failed but branch was pushed upstream")
	}
}

func TestGuardBoundaryAnchoring(t *testing.T) {
	dir := t.TempDir()
	patch := `From abc Mon Sep 17 00:00:00 2001
Subject: x
--- a/secretarial/notes.txt
+++ b/secretarial/notes.txt
@@ -1 +1 @@
-a
+b
`
	if err := os.WriteFile(filepath.Join(dir, "0001-x.patch"), []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	// "secretarial" must NOT match excluded path "secret" ((/|$) anchoring).
	if err := GuardPatches(dir, []string{"secret"}); err != nil {
		t.Fatalf("false positive on path-prefix sibling: %v", err)
	}
	// But "secret/x" must match.
	patch2 := strings.ReplaceAll(patch, "secretarial/notes.txt", "secret/notes.txt")
	if err := os.WriteFile(filepath.Join(dir, "0002-x.patch"), []byte(patch2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GuardPatches(dir, []string{"secret"}); err == nil {
		t.Fatal("guard missed an excluded path")
	}
	// Deletions (--- a/...) must be caught too.
	dir2 := t.TempDir()
	patch3 := `Subject: del
--- a/secret/key.txt
+++ /dev/null
@@ -1 +0,0 @@
-gone
`
	if err := os.WriteFile(filepath.Join(dir2, "0001-del.patch"), []byte(patch3), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GuardPatches(dir2, []string{"secret"}); err == nil {
		t.Fatal("guard missed a deletion of an excluded file")
	}
}

// --- §13.6 rejection + conflict flows ---

func TestMergeCommitRejected(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "side")
	f.commit(agent, "public/side.txt", "side\n", "side work")
	f.git(agent, "checkout", "-b", "merged", "origin/main")
	f.commit(agent, "public/m.txt", "m\n", "main-ish work")
	f.git(agent, "merge", "--no-ff", "-m", "merge side", "side")
	f.git(agent, "push", "origin", "merged")

	_, err := f.eng.Promote(context.Background(), f.bridge, "merged", "main", "")
	var rej *ErrRejected
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "rebase onto main") {
		t.Fatalf("expected merge rejection with remediation, got %v", err)
	}
}

func TestEmptyBranchRejected(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "noop")
	f.git(agent, "push", "origin", "noop")

	_, err := f.eng.Promote(context.Background(), f.bridge, "noop", "main", "")
	var rej *ErrRejected
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "nothing to promote") {
		t.Fatalf("expected empty-branch rejection, got %v", err)
	}
}

func TestAmConflictNeedsAttentionAndAbort(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/app.txt", "line one CHANGED\nline two\n", "agent: edit app")
	f.git(agent, "push", "origin", "feature")

	// Sabotage the commit-map so the base resolves to the very first real
	// commit, where public/app.txt's context differs → guaranteed am conflict.
	pf, err := f.eng.RunPreflight(context.Background(), f.bridge, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	rootReal := f.git(f.src, "rev-list", "--max-parents=0", "main")
	mapPath := filepath.Join(f.workdir, "test", "commit-map")
	data, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(l)
		if len(fields) == 2 && fields[1] == pf.BaseFiltered {
			continue // drop the genuine mapping
		}
		lines = append(lines, l)
	}
	lines = append(lines, rootReal+" "+pf.BaseFiltered)
	if err := os.WriteFile(mapPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The root commit has no readme; the agent's patch still applies — make
	// the patch touch content that differs at root: app.txt at root is
	// identical... so instead create real divergence: edit app.txt upstream
	// history? Simpler: agent patch edits readme which doesn't exist at root.
	f.git(agent, "checkout", "feature")
	f.commit(agent, "public/readme.md", "# readme EDITED\n", "agent: edit readme")
	f.git(agent, "push", "origin", "feature")

	_, err = f.eng.Promote(context.Background(), f.bridge, "feature", "main", "")
	var conflict *ErrAmConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected am conflict, got %v\nlog:\n%s", err, f.logs)
	}
	if !strings.Contains(conflict.Recovery(), "git am --continue") {
		t.Fatal("recovery instructions missing")
	}
	// Workspace is mid-am; abort must clean it.
	work := filepath.Join(f.workdir, "test", "source-work")
	if _, err := os.Stat(filepath.Join(work, ".git", "rebase-apply")); err != nil {
		t.Fatal("expected am state to be left for manual resolution")
	}
	if err := f.eng.AbortPromotion(context.Background(), f.bridge); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, ".git", "rebase-apply")); err == nil {
		t.Fatal("abort did not clean am state")
	}
}

// --- §13.7 finalize detection ---

func TestFinalizeDetectsTrueMergeAndFastForward(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/feature.txt", "agent work\n", "agent: add feature")
	f.git(agent, "push", "origin", "feature")

	res, err := f.eng.Promote(context.Background(), f.bridge, "feature", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	landed, err := f.eng.DetectLanded(context.Background(), f.bridge, res.TipSHA, "main")
	if err != nil {
		t.Fatal(err)
	}
	if landed {
		t.Fatal("not merged yet, must not report landed")
	}
	// True merge upstream.
	f.git(f.dev, "fetch", "origin")
	f.git(f.dev, "merge", "--no-ff", "-m", "merge feature", "origin/feature")
	f.git(f.dev, "push", "origin", "main")

	landed, err = f.eng.DetectLanded(context.Background(), f.bridge, res.TipSHA, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !landed {
		t.Fatal("true merge not detected")
	}
	if err := f.eng.DeleteUpstreamBranch(context.Background(), f.bridge, res.RealBranch, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gitErr(f.src, "rev-parse", "--verify", "refs/heads/feature"); err == nil {
		t.Fatal("upstream ai/ branch not deleted")
	}
}

func TestFinalizeDetectsRebaseMergeViaCherry(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/feature.txt", "agent work\n", "agent: add feature")
	f.git(agent, "push", "origin", "feature")

	res, err := f.eng.Promote(context.Background(), f.bridge, "feature", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	// Upstream advances, then the ai/ branch is rebase-merged (cherry-picked).
	f.commit(f.dev, "public/other.txt", "other\n", "unrelated upstream work")
	f.git(f.dev, "fetch", "origin")
	f.git(f.dev, "cherry-pick", res.TipSHA)
	f.git(f.dev, "push", "origin", "main")

	landed, err := f.eng.DetectLanded(context.Background(), f.bridge, res.TipSHA, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !landed {
		t.Fatal("rebase-merge not detected via git cherry")
	}
}

func TestSquashMergeIsNotAutoDetected(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/f1.txt", "one\n", "agent: part 1")
	f.commit(agent, "public/f2.txt", "two\n", "agent: part 2")
	f.git(agent, "push", "origin", "feature")

	res, err := f.eng.Promote(context.Background(), f.bridge, "feature", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	// Squash merge upstream: one combined commit, different patch-ids.
	f.git(f.dev, "fetch", "origin")
	f.git(f.dev, "merge", "--squash", "origin/feature")
	f.git(f.dev, "commit", "-m", "squash: feature")
	f.git(f.dev, "push", "origin", "main")

	landed, err := f.eng.DetectLanded(context.Background(), f.bridge, res.TipSHA, "main")
	if err != nil {
		t.Fatal(err)
	}
	if landed {
		t.Fatal("squash merge must not be auto-detected (manual finalize only)")
	}
}

// --- misc engine behavior ---

func TestPromotionWithBinaryFile(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "bin")
	full := filepath.Join(agent, "public", "blob.bin")
	if err := os.WriteFile(full, []byte{0x00, 0x01, 0xff, 0xfe, 0x00, 0x42}, 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(agent, "add", "--", "public/blob.bin")
	f.git(agent, "commit", "-m", "agent: add binary")
	f.git(agent, "push", "origin", "bin")

	if _, err := f.eng.Promote(context.Background(), f.bridge, "bin", "main", ""); err != nil {
		t.Fatalf("binary promotion failed: %v\nlog:\n%s", err, f.logs)
	}
	files := f.git(f.src, "ls-tree", "-r", "--name-only", "bin")
	if !strings.Contains(files, "public/blob.bin") {
		t.Fatal("binary file missing upstream")
	}
}

func TestCommitMapMissingFailsWithGuidance(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()

	agent := f.agentClone("agent")
	f.git(agent, "checkout", "-b", "feature")
	f.commit(agent, "public/feature.txt", "x\n", "agent: x")
	f.git(agent, "push", "origin", "feature")

	if err := os.Remove(filepath.Join(f.workdir, "test", "commit-map")); err != nil {
		t.Fatal(err)
	}
	_, err := f.eng.Promote(context.Background(), f.bridge, "feature", "main", "")
	if err == nil || !strings.Contains(err.Error(), "run a sync first") {
		t.Fatalf("expected 'run a sync first' guidance, got %v", err)
	}
}

func TestCleanWorkspaceAbortsStaleAmState(t *testing.T) {
	f := newFixture(t)
	f.initAndSync()
	work := filepath.Join(f.workdir, "test", "source-work")
	// Fake a stale am session.
	if err := os.MkdirAll(filepath.Join(work, ".git", "rebase-apply"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.eng.CleanWorkspace(context.Background(), f.bridge)
	if _, err := os.Stat(filepath.Join(work, ".git", "rebase-apply")); err == nil {
		t.Fatal("stale am state not cleaned")
	}
}

// tgit / tgitErr run git with a fixed identity and no ambient config, for the
// standalone tests below that don't use the filter-repo fixture.
func tgitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
}

func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = tgitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func tgitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = tgitEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestFetchMirrorObjectsEnables3WayMerge reproduces the promotion failure where
// `git am --3way` cannot reconstruct the base tree because the patch's recorded
// blobs live only on the Gitea mirror, and asserts fetchMirrorObjects fixes it.
func TestFetchMirrorObjectsEnables3WayMerge(t *testing.T) {
	dir := t.TempDir()
	write := func(repo, name, content string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Mirror upstream: base A/B/C on main; an agent branch appends D.
	mo := filepath.Join(dir, "mirror-origin")
	if err := os.MkdirAll(mo, 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, mo, "init", "-q", "-b", "main", ".")
	write(mo, "f.txt", "A\nB\nC\n")
	tgit(t, mo, "add", ".")
	tgit(t, mo, "commit", "-qm", "base")
	tgit(t, mo, "checkout", "-q", "-b", "agent")
	write(mo, "f.txt", "A\nB\nC\nD\n")
	tgit(t, mo, "commit", "-qam", "agent append D")
	tgit(t, mo, "checkout", "-q", "main")

	// Source upstream: base diverges (B -> B0) so the agent patch cannot apply
	// directly, but IS 3-way-mergeable using the mirror's base blob.
	so := filepath.Join(dir, "source-origin")
	if err := os.MkdirAll(so, 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, so, "init", "-q", "-b", "main", ".")
	write(so, "f.txt", "A\nB0\nC\n")
	tgit(t, so, "add", ".")
	tgit(t, so, "commit", "-qm", "source base")

	// Engine workspace layout: gitea-clone (mirror) + source-work (source).
	workdir := filepath.Join(dir, "work")
	slug := "b"
	ws := filepath.Join(workdir, slug)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, ws, "clone", "-q", mo, "gitea-clone")
	tgit(t, ws, "clone", "-q", so, "source-work")
	gc := filepath.Join(ws, "gitea-clone")
	sw := filepath.Join(ws, "source-work")

	// Sluice's export step: patches generated from the mirror.
	patches := filepath.Join(dir, "patches")
	if err := os.MkdirAll(patches, 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, gc, "format-patch", "--binary", "-o", patches, "origin/main..origin/agent")
	pfiles, _ := filepath.Glob(filepath.Join(patches, "*.patch"))
	if len(pfiles) == 0 {
		t.Fatal("no patches generated")
	}
	amArgs := append([]string{"am", "--3way"}, pfiles...)

	// Without the mirror objects, the 3-way fallback cannot reconstruct the base.
	tgit(t, sw, "checkout", "-q", "-B", "promoted")
	if _, err := tgitErr(sw, amArgs...); err == nil {
		_, _ = tgitErr(sw, "am", "--abort")
		t.Fatal("expected git am --3way to fail without mirror objects")
	}
	_, _ = tgitErr(sw, "am", "--abort")

	// The fix: fetch the mirror's objects into source-work, then it merges.
	eng := New(workdir, "", "", &execx.Runner{Log: func(string) {}})
	if err := eng.fetchMirrorObjects(context.Background(), &Bridge{Slug: slug}); err != nil {
		t.Fatalf("fetchMirrorObjects: %v", err)
	}
	tgit(t, sw, "checkout", "-q", "-B", "promoted2")
	if out, err := tgitErr(sw, amArgs...); err != nil {
		t.Fatalf("git am --3way should succeed after fetch: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(sw, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A\nB0\nC\nD\n" {
		t.Fatalf("merged content = %q, want B0 kept and D appended", got)
	}
}

// TestCleanWorkspaceForceRemovesUnabortableAmState covers the wedged case the
// user hit: `git am --abort` cannot clear .git/rebase-apply (here because
// source-work is not a valid repo, standing in for the ownership/permission
// failure), so CleanWorkspace must force-remove it rather than leave every
// future `git am` blocked by "previous rebase directory still exists".
func TestCleanWorkspaceForceRemovesUnabortableAmState(t *testing.T) {
	dir := t.TempDir()
	workdir := filepath.Join(dir, "work")
	slug := "b"
	rebaseApply := filepath.Join(workdir, slug, "source-work", ".git", "rebase-apply")
	if err := os.MkdirAll(rebaseApply, 0o755); err != nil {
		t.Fatal(err)
	}
	eng := New(workdir, "", "", &execx.Runner{Log: func(string) {}})
	eng.CleanWorkspace(context.Background(), &Bridge{Slug: slug})
	if _, err := os.Stat(rebaseApply); err == nil {
		t.Fatal("wedged rebase-apply not force-removed after git am --abort could not clear it")
	}
}

func TestValidateExcludedPath(t *testing.T) {
	valid := []string{"secret", "a/b", "deep/nested/dir", "with-dash_underscore"}
	for _, p := range valid {
		if err := ValidateExcludedPath(p); err != nil {
			t.Errorf("%q should be valid: %v", p, err)
		}
	}
	invalid := []string{"", " lead", "trail ", "/abs", "a/../b", "..", "a.b", "a*b", "a(b", "a|b", "a\\b", "a$b", "a\tb", "dir/"}
	for _, p := range invalid {
		if err := ValidateExcludedPath(p); err == nil {
			t.Errorf("%q should be rejected", p)
		}
	}
}
