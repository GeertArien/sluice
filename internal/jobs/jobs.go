// Package jobs orchestrates background work: a DB-backed queue with a
// bounded worker pool, per-bridge serialization (spec §5.3/§7), cron
// schedules and webhook debouncing, and the sync/promote/finalize/init/
// verify job implementations.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/geertarien/sluice/internal/engine"
	"github.com/geertarien/sluice/internal/execx"
	"github.com/geertarien/sluice/internal/gitea"
	"github.com/geertarien/sluice/internal/secrets"
	"github.com/geertarien/sluice/internal/sshkey"
	"github.com/geertarien/sluice/internal/store"
)

// GiteaAPI is the slice of the Gitea client the jobs layer uses;
// an interface so tests can substitute a fake.
type GiteaAPI interface {
	EnsureRepo(ctx context.Context, owner, repo string) (*gitea.Repo, error)
	CheckToken(ctx context.Context) error
	OpenPRs(ctx context.Context, owner, repo string) ([]gitea.PR, error)
	FindOpenPRByHead(ctx context.Context, owner, repo, branch string) (*gitea.PR, error)
	ClosePR(ctx context.Context, owner, repo string, index int64) error
	CommentOnPR(ctx context.Context, owner, repo string, index int64, body string) error
	DeleteBranch(ctx context.Context, owner, repo, branch string) error
}

type Service struct {
	Store      *store.Store
	Box        *secrets.Box
	Workdir    string
	KnownHosts string
	Workers    int

	// NewGitea builds an API client; overridable in tests.
	NewGitea func(baseURL, token string) GiteaAPI

	notify      chan struct{}
	cron        *cron.Cron
	cronEntries map[int64]cron.EntryID
	cronMu      sync.Mutex

	bridgeLocks   map[int64]*sync.Mutex
	bridgeLocksMu sync.Mutex

	debounce   map[int64]*time.Timer
	debounceMu sync.Mutex
	// WebhookDebounce coalesces webhook bursts (spec §5.3, ~30s).
	WebhookDebounce time.Duration
}

func New(st *store.Store, box *secrets.Box, workdir, knownHosts string, workers int) *Service {
	if workers <= 0 {
		workers = 4
	}
	return &Service{
		Store:      st,
		Box:        box,
		Workdir:    workdir,
		KnownHosts: knownHosts,
		Workers:    workers,
		NewGitea: func(baseURL, token string) GiteaAPI {
			return gitea.New(baseURL, token)
		},
		notify:          make(chan struct{}, 64),
		cron:            cron.New(),
		cronEntries:     map[int64]cron.EntryID{},
		bridgeLocks:     map[int64]*sync.Mutex{},
		debounce:        map[int64]*time.Timer{},
		WebhookDebounce: 30 * time.Second,
	}
}

func (s *Service) BridgeLock(id int64) *sync.Mutex {
	s.bridgeLocksMu.Lock()
	defer s.bridgeLocksMu.Unlock()
	if s.bridgeLocks[id] == nil {
		s.bridgeLocks[id] = &sync.Mutex{}
	}
	return s.bridgeLocks[id]
}

// Start launches the worker pool and the cron scheduler.
func (s *Service) Start(ctx context.Context) {
	for i := 0; i < s.Workers; i++ {
		go s.workerLoop(ctx)
	}
	s.ReloadSchedules()
	s.cron.Start()
	go func() {
		<-ctx.Done()
		s.cron.Stop()
	}()
}

// Enqueue creates a job and wakes the workers.
func (s *Service) Enqueue(bridgeID int64, kind string, payload any) (int64, error) {
	id, err := s.Store.EnqueueJob(bridgeID, kind, payload)
	if err == nil {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
	return id, err
}

// WebhookSync schedules a debounced sync for a bridge.
func (s *Service) WebhookSync(bridgeID int64) {
	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()
	if t, ok := s.debounce[bridgeID]; ok {
		t.Reset(s.WebhookDebounce)
		return
	}
	s.debounce[bridgeID] = time.AfterFunc(s.WebhookDebounce, func() {
		s.debounceMu.Lock()
		delete(s.debounce, bridgeID)
		s.debounceMu.Unlock()
		if _, err := s.Enqueue(bridgeID, "sync", nil); err != nil {
			log.Printf("webhook sync enqueue failed for bridge %d: %v", bridgeID, err)
		}
		s.Store.Audit(bridgeID, "webhook", "sync_enqueued", nil)
	})
}

// ReloadSchedules rebuilds cron entries from bridge settings.
func (s *Service) ReloadSchedules() {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	for _, id := range s.cronEntries {
		s.cron.Remove(id)
	}
	s.cronEntries = map[int64]cron.EntryID{}
	bridges, err := s.Store.Bridges()
	if err != nil {
		log.Printf("reload schedules: %v", err)
		return
	}
	for _, b := range bridges {
		if b.ScheduleCron == "" || b.Status != "active" {
			continue
		}
		bid := b.ID
		entry, err := s.cron.AddFunc(b.ScheduleCron, func() {
			if _, err := s.Enqueue(bid, "sync", nil); err != nil {
				log.Printf("cron sync enqueue failed for bridge %d: %v", bid, err)
			} else {
				s.Store.Audit(bid, "scheduler", "sync_enqueued", nil)
			}
		})
		if err != nil {
			log.Printf("bridge %s: invalid cron %q: %v", b.Slug, b.ScheduleCron, err)
			continue
		}
		s.cronEntries[bid] = entry
	}
}

func (s *Service) workerLoop(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		job, err := s.Store.ClaimJob()
		if err == nil {
			s.runJob(ctx, job)
			// Finishing may unblock another job on the same bridge.
			select {
			case s.notify <- struct{}{}:
			default:
			}
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("claim job: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		case <-tick.C:
		}
	}
}

// runtimeBridge decrypts secrets and builds the engine view of a bridge.
func (s *Service) runtimeBridge(b *store.Bridge) (*engine.Bridge, string, error) {
	token := ""
	if len(b.GiteaTokenEnc) > 0 {
		var err error
		if token, err = s.Box.Decrypt(b.GiteaTokenEnc); err != nil {
			return nil, "", fmt.Errorf("decrypt gitea token: %w", err)
		}
	}
	return &engine.Bridge{
		Slug:               b.Slug,
		SourceRemoteURL:    b.SourceRemoteURL,
		GiteaSSHURL:        b.GiteaSSHURL,
		ExcludedPaths:      b.ExcludedPaths,
		SyncBranches:       b.SyncBranches,
		SyncGlobs:          b.SyncGlobs,
		TripwireStrings:    b.TripwireStrings,
		PromoteName:        b.PromoteName,
		PromoteEmail:       b.PromoteEmail,
		PromoteKeepTrailer: b.PromoteKeepTrailer,
		PromoteSignoff:     b.PromoteSignoff,
	}, token, nil
}

// EngineFor builds an engine whose output is appended to the given job log.
// The decrypted token, webhook secret and any per-bridge SSH key are
// registered as scrub targets. A configured SSH key is materialized to a
// 0600 file in the bridge workspace and used as ssh's sole identity.
func (s *Service) EngineFor(b *store.Bridge, token string, logSink func(string)) *engine.Engine {
	secretsToScrub := []string{token}
	if len(b.WebhookSecretEnc) > 0 {
		if ws, err := s.Box.Decrypt(b.WebhookSecretEnc); err == nil {
			secretsToScrub = append(secretsToScrub, ws)
		}
	}
	sshKeyPath := ""
	if len(b.SSHPrivateKeyEnc) > 0 {
		if key, err := s.Box.Decrypt(b.SSHPrivateKeyEnc); err == nil {
			secretsToScrub = append(secretsToScrub, key)
			dir := filepath.Join(s.Workdir, b.Slug)
			if err := os.MkdirAll(dir, 0o700); err == nil {
				p := filepath.Join(dir, ".ssh_id")
				if err := os.WriteFile(p, []byte(sshkey.EnsureTrailingNewline(key)), 0o600); err == nil {
					sshKeyPath = p
				} else {
					logSink("warning: could not write per-bridge SSH key: " + err.Error())
				}
			}
		} else {
			logSink("warning: could not decrypt per-bridge SSH key: " + err.Error())
		}
	}
	runner := &execx.Runner{Log: logSink, Secrets: secretsToScrub}
	return engine.New(s.Workdir, s.KnownHosts, sshKeyPath, runner)
}

type promotePayload struct {
	Branch string `json:"branch"`
	Base   string `json:"base"`
}

type finalizePayload struct {
	PromotionID int64 `json:"promotion_id,omitempty"` // 0 → detection pass over all
	Manual      bool  `json:"manual,omitempty"`
}

// ManualFinalizePayload builds the payload for a manual "Mark as merged"
// finalize job (squash merges, spec §5.5).
func ManualFinalizePayload(promotionID int64) any {
	return finalizePayload{PromotionID: promotionID, Manual: true}
}

// RuntimeBridge exposes the decrypted engine view for the web layer
// (synchronous preflight and abort actions).
func (s *Service) RuntimeBridge(b *store.Bridge) (*engine.Bridge, string, error) {
	return s.runtimeBridge(b)
}

func (s *Service) runJob(ctx context.Context, job *store.Job) {
	bridge, err := s.Store.BridgeByID(job.BridgeID)
	if err != nil {
		_ = s.Store.FinishJob(job.ID, "failed", "bridge not found: "+err.Error())
		return
	}
	lock := s.BridgeLock(bridge.ID)
	lock.Lock()
	defer lock.Unlock()

	logSink := func(line string) {
		_ = s.Store.AppendJobLog(job.ID, line+"\n")
	}
	rb, token, err := s.runtimeBridge(bridge)
	if err != nil {
		_ = s.Store.FinishJob(job.ID, "failed", err.Error())
		return
	}
	eng := s.EngineFor(bridge, token, logSink)
	api := s.NewGitea(bridge.GiteaBaseURL, token)

	eng.CleanWorkspace(ctx, rb)

	var jobErr error
	status := "success"
	switch job.Kind {
	case "init":
		jobErr = s.runInit(ctx, bridge, rb, eng, api)
	case "sync":
		jobErr = s.runSync(ctx, bridge, rb, eng, api)
	case "verify":
		jobErr = s.runVerify(ctx, bridge, rb, eng)
	case "promote":
		var p promotePayload
		if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
			jobErr = fmt.Errorf("bad payload: %w", err)
			break
		}
		var needsAttention bool
		needsAttention, jobErr = s.runPromote(ctx, bridge, rb, eng, api, p.Branch, p.Base, logSink)
		if needsAttention {
			status = "needs_attention"
		}
	case "finalize":
		var fp finalizePayload
		_ = json.Unmarshal([]byte(job.Payload), &fp)
		if fp.PromotionID != 0 {
			// Manual "Mark as merged" (squash merges, spec §5.5).
			var p *store.Promotion
			if p, jobErr = s.Store.PromotionByID(fp.PromotionID); jobErr == nil {
				jobErr = s.FinalizePromotion(ctx, bridge, rb, eng, api, p, fp.Manual)
			}
		} else {
			jobErr = s.runFinalize(ctx, bridge, rb, eng, api)
		}
	default:
		jobErr = fmt.Errorf("unknown job kind %q", job.Kind)
	}

	if jobErr != nil {
		if status != "needs_attention" {
			status = "failed"
		}
		logSink("ERROR: " + jobErr.Error())
		_ = s.Store.FinishJob(job.ID, status, jobErr.Error())
		return
	}
	_ = s.Store.FinishJob(job.ID, status, "")
}

func (s *Service) runInit(ctx context.Context, bridge *store.Bridge, rb *engine.Bridge, eng *engine.Engine, api GiteaAPI) error {
	log := eng.Runner.Log
	log("== init: validating connectivity ==")
	if err := api.CheckToken(ctx); err != nil {
		return fmt.Errorf("gitea token check failed: %w", err)
	}
	if _, err := eng.Runner.Run(ctx, "", "git", "ls-remote", "--heads", "--", bridge.SourceRemoteURL); err != nil {
		return fmt.Errorf("source remote not reachable: %w", err)
	}
	log("== init: ensuring Gitea repo exists ==")
	repo, err := api.EnsureRepo(ctx, bridge.GiteaOwner, bridge.GiteaRepo)
	if err != nil {
		return err
	}
	if repo.SSHURL != "" && bridge.GiteaSSHURL == "" {
		bridge.GiteaSSHURL = repo.SSHURL
		rb.GiteaSSHURL = repo.SSHURL
		_ = s.Store.UpdateBridge(bridge)
	}
	log("== init: creating workspace clones ==")
	if err := eng.InitWorkspace(ctx, rb); err != nil {
		return err
	}
	log("== init: first sync ==")
	if err := eng.Sync(ctx, rb); err != nil {
		return err
	}
	_ = s.Store.SetBridgeSyncResult(bridge.ID, true)
	log("== init: verification ==")
	if err := s.runVerify(ctx, bridge, rb, eng); err != nil {
		return err
	}
	s.Store.Audit(bridge.ID, "admin", "bridge_initialized", map[string]any{"slug": bridge.Slug})
	log("init complete — review the verification result, then activate the bridge")
	return nil
}

func (s *Service) runSync(ctx context.Context, bridge *store.Bridge, rb *engine.Bridge, eng *engine.Engine, api GiteaAPI) error {
	err := eng.Sync(ctx, rb)
	_ = s.Store.SetBridgeSyncResult(bridge.ID, err == nil)
	if err != nil {
		return err
	}
	s.Store.Audit(bridge.ID, "admin", "sync_completed", nil)
	// Spec §5.3: every successful sync runs finalization checks.
	if ferr := s.runFinalize(ctx, bridge, rb, eng, api); ferr != nil {
		eng.Runner.Log("finalization pass after sync failed: " + ferr.Error())
	}
	return nil
}

func (s *Service) runVerify(ctx context.Context, bridge *store.Bridge, rb *engine.Bridge, eng *engine.Engine) error {
	res, err := eng.Verify(ctx, rb)
	if err != nil {
		_ = s.Store.SetBridgeVerifyResult(bridge.ID, false)
		return err
	}
	_ = s.Store.SetBridgeVerifyResult(bridge.ID, res.OK)
	s.Store.Audit(bridge.ID, "admin", "verification", map[string]any{
		"ok": res.OK, "path_findings": res.PathFindings, "tripwire_findings": res.TripwireFindings,
	})
	if !res.OK {
		return fmt.Errorf("verification FAILED: excluded paths %v, tripwires %v", res.PathFindings, res.TripwireFindings)
	}
	return nil
}

// runPromote returns needsAttention=true when the job should end in
// needs_attention (am conflict) rather than failed.
func (s *Service) runPromote(ctx context.Context, bridge *store.Bridge, rb *engine.Bridge, eng *engine.Engine, api GiteaAPI, branch, base string, logSink func(string)) (bool, error) {
	// Find the matching open PR (nullable: branch-only promotions are allowed).
	var prNumber *int64
	if pr, err := api.FindOpenPRByHead(ctx, bridge.GiteaOwner, bridge.GiteaRepo, branch); err != nil {
		logSink("note: could not query Gitea PRs (continuing branch-only): " + err.Error())
	} else if pr != nil {
		prNumber = &pr.Number
	}

	recordRejected := func(reason string) {
		p := &store.Promotion{
			BridgeID: bridge.ID, GiteaBranch: branch, GiteaPRNumber: prNumber,
			RealBranch: "ai/" + branch, BaseBranch: base, Status: "rejected",
		}
		_ = s.Store.CreatePromotion(p)
		s.Store.Audit(bridge.ID, "admin", "promotion_rejected", map[string]any{"branch": branch, "reason": reason})
	}

	res, err := eng.Promote(ctx, rb, branch, base)
	if err != nil {
		var guard *engine.ErrGuardViolation
		if errors.As(err, &guard) {
			logSink("SECURITY GUARD FAILED — promotion rejected, nothing was pushed upstream.")
			logSink("Offending diff headers:\n" + strings.Join(guard.Lines, "\n"))
			recordRejected(guard.Error())
			return false, err
		}
		var rej *engine.ErrRejected
		if errors.As(err, &rej) {
			recordRejected(rej.Reason)
			return false, err
		}
		var conflict *engine.ErrAmConflict
		if errors.As(err, &conflict) {
			p := &store.Promotion{
				BridgeID: bridge.ID, GiteaBranch: branch, GiteaPRNumber: prNumber,
				RealBranch: conflict.RealBranch, BaseBranch: base, Status: "conflict",
			}
			_ = s.Store.CreatePromotion(p)
			s.Store.Audit(bridge.ID, "admin", "promotion_conflict", map[string]any{"branch": branch})
			logSink(conflict.Recovery())
			return true, err
		}
		return false, err
	}

	p := &store.Promotion{
		BridgeID: bridge.ID, GiteaBranch: branch, GiteaPRNumber: prNumber,
		RealBranch: res.RealBranch, RealTipSHA: res.TipSHA, BaseBranch: base, Status: "promoted",
	}
	if err := s.Store.CreatePromotion(p); err != nil {
		return false, err
	}
	s.Store.Audit(bridge.ID, "admin", "promotion_succeeded", map[string]any{
		"branch": branch, "real_branch": res.RealBranch, "tip": res.TipSHA, "commits": res.NumCommits,
	})
	if prNumber != nil {
		msg := fmt.Sprintf("Promoted upstream as `%s` (%d commit(s), tip `%s`). "+
			"This PR will be closed automatically once the change lands upstream.",
			res.RealBranch, res.NumCommits, res.TipSHA[:12])
		if err := api.CommentOnPR(ctx, bridge.GiteaOwner, bridge.GiteaRepo, *prNumber, msg); err != nil {
			logSink("note: failed to comment on Gitea PR: " + err.Error())
		}
	}
	if cu := CompareURL(bridge.SourceRemoteURL, base, res.RealBranch); cu != "" {
		logSink("compare URL: " + cu)
	}
	logSink(fmt.Sprintf("promotion complete: %s @ %s", res.RealBranch, res.TipSHA))
	return false, nil
}

func (s *Service) runFinalize(ctx context.Context, bridge *store.Bridge, rb *engine.Bridge, eng *engine.Engine, api GiteaAPI) error {
	promos, err := s.Store.PromotionsByStatus(bridge.ID, "promoted")
	if err != nil {
		return err
	}
	if len(promos) == 0 {
		eng.Runner.Log("finalize: no promotions awaiting upstream merge")
		return nil
	}
	var firstErr error
	for _, p := range promos {
		landed, err := eng.DetectLanded(ctx, rb, p.RealTipSHA, p.BaseBranch)
		if err != nil {
			eng.Runner.Log(fmt.Sprintf("finalize: detection failed for %s: %v", p.RealBranch, err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !landed {
			eng.Runner.Log(fmt.Sprintf("finalize: %s not landed yet (squash merges need manual finalize)", p.RealBranch))
			continue
		}
		if err := s.FinalizePromotion(ctx, bridge, rb, eng, api, p, false); err != nil {
			eng.Runner.Log(fmt.Sprintf("finalize: cleanup failed for %s: %v", p.RealBranch, err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// FinalizePromotion performs the §12.3 cleanup. manual=true is the
// "Mark as merged" button (squash merges).
func (s *Service) FinalizePromotion(ctx context.Context, bridge *store.Bridge, rb *engine.Bridge, eng *engine.Engine, api GiteaAPI, p *store.Promotion, manual bool) error {
	logf := eng.Runner.Log
	if p.GiteaPRNumber != nil {
		msg := fmt.Sprintf("This change landed upstream (promoted as `%s`). "+
			"Closing this PR rather than merging it: the mirror receives the synced-back commits "+
			"with different SHAs, so a merge here is neither possible nor needed. "+
			"Your work is upstream — this is a success, not a rejection.", p.RealBranch)
		if err := api.CommentOnPR(ctx, bridge.GiteaOwner, bridge.GiteaRepo, *p.GiteaPRNumber, msg); err != nil {
			logf("finalize: PR comment failed: " + err.Error())
		}
		if err := api.ClosePR(ctx, bridge.GiteaOwner, bridge.GiteaRepo, *p.GiteaPRNumber); err != nil {
			logf("finalize: PR close failed: " + err.Error())
		}
	}
	if err := api.DeleteBranch(ctx, bridge.GiteaOwner, bridge.GiteaRepo, p.GiteaBranch); err != nil {
		logf("finalize: gitea branch delete failed (may already be gone): " + err.Error())
	}
	if err := eng.DeleteUpstreamBranch(ctx, rb, p.RealBranch); err != nil {
		logf("finalize: upstream branch delete failed (may already be gone): " + err.Error())
	}
	if err := s.Store.UpdatePromotionStatus(p.ID, "finalized"); err != nil {
		return err
	}
	actor := "scheduler"
	if manual {
		actor = "admin"
	}
	s.Store.Audit(bridge.ID, actor, "promotion_finalized", map[string]any{
		"branch": p.GiteaBranch, "real_branch": p.RealBranch, "manual": manual,
	})
	logf(fmt.Sprintf("finalized promotion of %s (upstream branch %s cleaned up)", p.GiteaBranch, p.RealBranch))
	return nil
}

var (
	githubSSHRe = regexp.MustCompile(`^git@github\.com:([^/]+)/(.+?)(\.git)?$`)
	gitlabSSHRe = regexp.MustCompile(`^git@gitlab\.com:([^/]+)/(.+?)(\.git)?$`)
)

// CompareURL builds a copyable compare link when the source host is
// recognizable (spec §5.4).
func CompareURL(sourceURL, base, branch string) string {
	if m := githubSSHRe.FindStringSubmatch(sourceURL); m != nil {
		return fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s", m[1], m[2], base, url.PathEscape(branch))
	}
	if m := gitlabSSHRe.FindStringSubmatch(sourceURL); m != nil {
		return fmt.Sprintf("https://gitlab.com/%s/%s/-/compare/%s...%s", m[1], m[2], base, url.PathEscape(branch))
	}
	if u, err := url.Parse(sourceURL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
		switch {
		case strings.Contains(u.Host, "github"):
			return fmt.Sprintf("%s://%s%s/compare/%s...%s", u.Scheme, u.Host, path, base, url.PathEscape(branch))
		case strings.Contains(u.Host, "gitlab"):
			return fmt.Sprintf("%s://%s%s/-/compare/%s...%s", u.Scheme, u.Host, path, base, url.PathEscape(branch))
		case strings.Contains(u.Host, "gitea"):
			return fmt.Sprintf("%s://%s%s/compare/%s...%s", u.Scheme, u.Host, path, base, url.PathEscape(branch))
		}
	}
	return ""
}
