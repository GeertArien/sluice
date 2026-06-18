package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/geertarien/sluice/internal/engine"
	"github.com/geertarien/sluice/internal/gitea"
	"github.com/geertarien/sluice/internal/jobs"
	"github.com/geertarien/sluice/internal/store"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// splitList parses a textarea/comma-separated field into trimmed entries.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) bridgeFromPath(w http.ResponseWriter, r *http.Request) *store.Bridge {
	b, err := s.Store.BridgeBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	return b
}

// liveOpenPRs fetches the bridge's open Gitea PRs, tolerating failure.
func (s *Server) liveOpenPRs(ctx context.Context, b *store.Bridge) ([]gitea.PR, error) {
	_, token, err := s.Jobs.RuntimeBridge(b)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	api := s.Jobs.NewGitea(b.GiteaBaseURL, token)
	return api.OpenPRs(cctx, b.GiteaOwner, b.GiteaRepo)
}

// ---------- dashboard ----------

type bridgeRow struct {
	Bridge         *store.Bridge
	OpenPRs        int
	OpenPRsErr     string
	AwaitingMerge  int
	NeedsAttention int
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	bridges, err := s.Store.Bridges()
	if err != nil {
		httpError(w, 500, "load bridges: %v", err)
		return
	}
	rows := make([]*bridgeRow, len(bridges))
	for i, b := range bridges {
		row := &bridgeRow{Bridge: b}
		row.AwaitingMerge, row.NeedsAttention, _ = s.Store.DashboardCounts(b.ID)
		if prs, err := s.liveOpenPRs(r.Context(), b); err != nil {
			row.OpenPRsErr = "API unreachable"
		} else {
			row.OpenPRs = len(prs)
		}
		rows[i] = row
	}
	s.renderPage(w, r, "dashboard.html", map[string]any{"Rows": rows})
}

// ---------- bridge create (init wizard, spec §5.1) ----------

func (s *Server) handleBridgeNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "bridge_new.html", map[string]any{"Form": url.Values{}})
}

func (s *Server) parseBridgeForm(r *http.Request, b *store.Bridge) error {
	b.Name = strings.TrimSpace(r.PostFormValue("name"))
	b.SourceRemoteURL = strings.TrimSpace(r.PostFormValue("source_remote_url"))
	b.GiteaBaseURL = strings.TrimRight(strings.TrimSpace(r.PostFormValue("gitea_base_url")), "/")
	b.GiteaOwner = strings.TrimSpace(r.PostFormValue("gitea_owner"))
	b.GiteaRepo = strings.TrimSpace(r.PostFormValue("gitea_repo"))
	b.GiteaSSHURL = strings.TrimSpace(r.PostFormValue("gitea_ssh_url"))
	b.ExcludedPaths = splitList(r.PostFormValue("excluded_paths"))
	b.SyncBranches = splitList(r.PostFormValue("sync_branches"))
	b.SyncGlobs = splitList(r.PostFormValue("sync_globs"))
	b.TripwireStrings = splitList(r.PostFormValue("tripwire_strings"))
	b.PromoteName = strings.TrimSpace(r.PostFormValue("promote_name"))
	b.PromoteEmail = strings.TrimSpace(r.PostFormValue("promote_email"))
	b.PromoteKeepTrailer = r.PostFormValue("promote_keep_trailer") == "on"
	b.PromoteSignoff = r.PostFormValue("promote_signoff") == "on"
	b.ScheduleCron = strings.TrimSpace(r.PostFormValue("schedule_cron"))

	if b.Name == "" || b.SourceRemoteURL == "" || b.GiteaBaseURL == "" || b.GiteaOwner == "" || b.GiteaRepo == "" {
		return errors.New("name, source remote, Gitea base URL, owner and repo are required")
	}
	if len(b.SyncBranches) == 0 {
		return errors.New("at least one sync branch is required")
	}
	if err := engine.ValidateExcludedPaths(b.ExcludedPaths); err != nil {
		return err
	}
	if b.PromoteName != "" && b.PromoteEmail == "" {
		return errors.New("promote email is required when a promote name is set")
	}
	return nil
}

func (s *Server) handleBridgeCreate(w http.ResponseWriter, r *http.Request) {
	b := &store.Bridge{Status: "paused"}
	formErr := s.parseBridgeForm(r, b)
	b.Slug = strings.TrimSpace(r.PostFormValue("slug"))
	if formErr == nil && !slugRe.MatchString(b.Slug) {
		formErr = errors.New("slug must be lowercase letters, digits and dashes (2-63 chars)")
	}
	token := r.PostFormValue("gitea_token")
	if formErr == nil && token == "" {
		formErr = errors.New("a Gitea API token is required")
	}
	if formErr != nil {
		s.renderPage(w, r, "bridge_new.html", map[string]any{"Error": formErr.Error(), "Form": r.PostForm})
		return
	}
	var err error
	if b.GiteaTokenEnc, err = s.Box.Encrypt(token); err != nil {
		httpError(w, 500, "encrypt token: %v", err)
		return
	}
	webhookSecret := randHex(24)
	if b.WebhookSecretEnc, err = s.Box.Encrypt(webhookSecret); err != nil {
		httpError(w, 500, "encrypt webhook secret: %v", err)
		return
	}
	if err := s.Store.CreateBridge(b); err != nil {
		s.renderPage(w, r, "bridge_new.html", map[string]any{"Error": "create failed (slug taken?): " + err.Error(), "Form": r.PostForm})
		return
	}
	s.Store.Audit(b.ID, "admin", "bridge_created", map[string]any{"slug": b.Slug})
	jobID, err := s.Jobs.Enqueue(b.ID, "init", nil)
	if err != nil {
		httpError(w, 500, "enqueue init: %v", err)
		return
	}
	http.Redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10), http.StatusSeeOther)
}

// ---------- bridge detail ----------

func (s *Server) handleBridgeDetail(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	jobsList, _ := s.Store.JobsForBridge(b.ID, 30)
	promotions, _ := s.Store.PromotionsForBridge(b.ID)
	attention, _ := s.Store.JobsNeedingAttention(b.ID)
	prs, prErr := s.liveOpenPRs(r.Context(), b)
	prErrMsg := ""
	if prErr != nil {
		prErrMsg = prErr.Error()
	}
	defaultBase := ""
	if len(b.SyncBranches) > 0 {
		defaultBase = b.SyncBranches[0]
	}
	s.renderPage(w, r, "bridge.html", map[string]any{
		"Bridge": b, "Jobs": jobsList, "Promotions": promotions,
		"Attention": attention, "PRs": prs, "PRErr": prErrMsg,
		"DefaultBase": defaultBase, "Tab": r.URL.Query().Get("tab"),
	})
}

// ---------- settings (spec §5.1 filter-change flow) ----------

func (s *Server) handleBridgeSettingsForm(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	webhookSecret := ""
	if len(b.WebhookSecretEnc) > 0 {
		webhookSecret, _ = s.Box.Decrypt(b.WebhookSecretEnc)
	}
	s.renderPage(w, r, "bridge_settings.html", map[string]any{
		"Bridge": b, "WebhookSecret": webhookSecret, "Error": "",
	})
}

func (s *Server) handleBridgeSettings(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	oldExcluded := strings.Join(b.ExcludedPaths, "\n")
	renderErr := func(msg string) {
		webhookSecret := ""
		if len(b.WebhookSecretEnc) > 0 {
			webhookSecret, _ = s.Box.Decrypt(b.WebhookSecretEnc)
		}
		s.renderPage(w, r, "bridge_settings.html", map[string]any{
			"Bridge": b, "WebhookSecret": webhookSecret, "Error": msg,
		})
	}
	if err := s.parseBridgeForm(r, b); err != nil {
		renderErr(err.Error())
		return
	}
	if token := r.PostFormValue("gitea_token"); token != "" {
		enc, err := s.Box.Encrypt(token)
		if err != nil {
			httpError(w, 500, "encrypt token: %v", err)
			return
		}
		b.GiteaTokenEnc = enc
	}
	filterChanged := strings.Join(b.ExcludedPaths, "\n") != oldExcluded
	if filterChanged {
		// Changing the filter rewrites ALL filtered SHAs; require typed
		// confirmation, then full re-sync and flag open promotions (§5.1).
		if r.PostFormValue("confirm_slug") != b.Slug {
			renderErr("You changed the excluded paths. This rewrites ALL filtered SHAs and orphans " +
				"existing agent branches on Gitea. Type the bridge slug in the confirmation field to proceed.")
			return
		}
	}
	if err := s.Store.UpdateBridge(b); err != nil {
		renderErr("save failed: " + err.Error())
		return
	}
	s.Store.Audit(b.ID, "admin", "bridge_updated", map[string]any{"filter_changed": filterChanged})
	s.Jobs.ReloadSchedules()
	if filterChanged {
		_ = s.Store.MarkOpenPromotionsNeedsAttention(b.ID)
		s.Store.Audit(b.ID, "admin", "filter_changed_resync", map[string]any{"excluded_paths": b.ExcludedPaths})
		if _, err := s.Jobs.Enqueue(b.ID, "sync", nil); err != nil {
			renderErr("saved, but re-sync enqueue failed: " + err.Error())
			return
		}
	}
	http.Redirect(w, r, "/bridges/"+b.Slug, http.StatusSeeOther)
}

func (s *Server) handleBridgeDelete(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	if r.PostFormValue("confirm_slug") != b.Slug {
		httpError(w, 400, "type the bridge slug to confirm deletion")
		return
	}
	if err := s.Store.DeleteBridge(b.ID); err != nil {
		httpError(w, 500, "delete: %v", err)
		return
	}
	_ = os.RemoveAll(filepath.Join(s.Jobs.Workdir, b.Slug))
	s.Store.Audit(b.ID, "admin", "bridge_deleted", map[string]any{"slug": b.Slug})
	s.Jobs.ReloadSchedules()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- lifecycle & job triggers ----------

func (s *Server) handleEnqueue(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := s.bridgeFromPath(w, r)
		if b == nil {
			return
		}
		jobID, err := s.Jobs.Enqueue(b.ID, kind, nil)
		if err != nil {
			httpError(w, 500, "enqueue %s: %v", kind, err)
			return
		}
		s.Store.Audit(b.ID, "admin", kind+"_enqueued", nil)
		http.Redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10), http.StatusSeeOther)
	}
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	// A bridge can only go active after a passing verification (spec §5.1).
	if b.LastVerifyOK == nil || !*b.LastVerifyOK {
		httpError(w, 400, "bridge cannot be activated: last verification missing or failed — run Verify first")
		return
	}
	_ = s.Store.SetBridgeStatus(b.ID, "active")
	s.Store.Audit(b.ID, "admin", "bridge_activated", nil)
	s.Jobs.ReloadSchedules()
	http.Redirect(w, r, "/bridges/"+b.Slug, http.StatusSeeOther)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	_ = s.Store.SetBridgeStatus(b.ID, "paused")
	s.Store.Audit(b.ID, "admin", "bridge_paused", nil)
	s.Jobs.ReloadSchedules()
	http.Redirect(w, r, "/bridges/"+b.Slug, http.StatusSeeOther)
}

// ---------- promotion (spec §5.4) ----------

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	branch := r.URL.Query().Get("branch")
	base := r.URL.Query().Get("base")
	if base == "" && len(b.SyncBranches) > 0 {
		base = b.SyncBranches[0]
	}
	if branch == "" || base == "" {
		httpError(w, 400, "branch and base are required")
		return
	}
	lock := s.Jobs.BridgeLock(b.ID)
	if !lock.TryLock() {
		s.renderPage(w, r, "preflight.html", map[string]any{
			"Bridge": b, "Branch": branch, "Base": base,
			"Busy": true,
		})
		return
	}
	defer lock.Unlock()

	rb, token, err := s.Jobs.RuntimeBridge(b)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	var logBuf strings.Builder
	eng := s.Jobs.EngineFor(b, token, func(line string) { logBuf.WriteString(line + "\n") })
	pf, err := eng.RunPreflight(r.Context(), rb, branch, base)
	data := map[string]any{
		"Bridge": b, "Branch": branch, "Base": base, "Preflight": pf,
		"PromoteIdentity": b.PromoteName != "",
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.renderPage(w, r, "preflight.html", data)
}

func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	b := s.bridgeFromPath(w, r)
	if b == nil {
		return
	}
	branch := strings.TrimSpace(r.PostFormValue("branch"))
	base := strings.TrimSpace(r.PostFormValue("base"))
	if branch == "" || base == "" {
		httpError(w, 400, "branch and base are required")
		return
	}
	jobID, err := s.Jobs.Enqueue(b.ID, "promote", map[string]string{"branch": branch, "base": base})
	if err != nil {
		httpError(w, 500, "enqueue promote: %v", err)
		return
	}
	s.Store.Audit(b.ID, "admin", "promote_enqueued", map[string]string{"branch": branch, "base": base})
	http.Redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10), http.StatusSeeOther)
}

// ---------- promotion lifecycle actions ----------

func (s *Server) promotionFromPath(w http.ResponseWriter, r *http.Request) (*store.Promotion, *store.Bridge) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil
	}
	p, err := s.Store.PromotionByID(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil
	}
	b, err := s.Store.BridgeByID(p.BridgeID)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil
	}
	return p, b
}

func (s *Server) handlePromotionFinalize(w http.ResponseWriter, r *http.Request) {
	p, b := s.promotionFromPath(w, r)
	if p == nil {
		return
	}
	if p.Status != "promoted" && p.Status != "needs_attention" {
		httpError(w, 400, "promotion is %s; only promoted ones can be marked merged", p.Status)
		return
	}
	jobID, err := s.Jobs.Enqueue(b.ID, "finalize", jobs.ManualFinalizePayload(p.ID))
	if err != nil {
		httpError(w, 500, "enqueue finalize: %v", err)
		return
	}
	http.Redirect(w, r, "/jobs/"+strconv.FormatInt(jobID, 10), http.StatusSeeOther)
}

func (s *Server) handlePromotionAbort(w http.ResponseWriter, r *http.Request) {
	p, b := s.promotionFromPath(w, r)
	if p == nil {
		return
	}
	if p.Status != "conflict" && p.Status != "needs_attention" {
		httpError(w, 400, "promotion is %s; nothing to abort", p.Status)
		return
	}
	lock := s.Jobs.BridgeLock(b.ID)
	if !lock.TryLock() {
		httpError(w, 409, "bridge is busy; retry when the running job finishes")
		return
	}
	defer lock.Unlock()
	rb, token, err := s.Jobs.RuntimeBridge(b)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	eng := s.Jobs.EngineFor(b, token, func(string) {})
	if err := eng.AbortPromotion(r.Context(), rb); err != nil {
		httpError(w, 500, "abort failed: %v", err)
		return
	}
	_ = s.Store.UpdatePromotionStatus(p.ID, "rejected")
	s.Store.Audit(b.ID, "admin", "promotion_aborted", map[string]any{"branch": p.GiteaBranch})
	http.Redirect(w, r, "/bridges/"+b.Slug+"?tab=promotions", http.StatusSeeOther)
}

func (s *Server) handlePromotionMarkPromoted(w http.ResponseWriter, r *http.Request) {
	p, b := s.promotionFromPath(w, r)
	if p == nil {
		return
	}
	if p.Status != "conflict" && p.Status != "needs_attention" {
		httpError(w, 400, "promotion is %s; manual promotion applies to conflicted ones", p.Status)
		return
	}
	tip := strings.ToLower(strings.TrimSpace(r.PostFormValue("tip_sha")))
	if !shaRe.MatchString(tip) {
		httpError(w, 400, "tip SHA must be a full 40-character commit hash")
		return
	}
	_ = s.Store.SetPromotionTip(p.ID, tip)
	_ = s.Store.UpdatePromotionStatus(p.ID, "promoted")
	s.Store.Audit(b.ID, "admin", "promotion_marked_manual", map[string]any{"branch": p.GiteaBranch, "tip": tip})
	http.Redirect(w, r, "/bridges/"+b.Slug+"?tab=promotions", http.StatusSeeOther)
}

// ---------- jobs & audit ----------

func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job, err := s.Store.JobByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	bridge, _ := s.Store.BridgeByID(job.BridgeID)
	canRetry := job.Kind == "sync" || job.Kind == "verify" || job.Kind == "init" || job.Kind == "finalize"
	s.renderPage(w, r, "job.html", map[string]any{
		"Job": job, "Bridge": bridge,
		"CanRetry": canRetry && (job.Status == "failed"),
		"Running":  job.Status == "queued" || job.Status == "running",
	})
}

func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job, err := s.Store.JobByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Job-Status", job.Status)
	_, _ = w.Write([]byte(job.Log))
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	var bridgeID int64
	bridgeSlug := r.URL.Query().Get("bridge")
	if bridgeSlug != "" {
		if b, err := s.Store.BridgeBySlug(bridgeSlug); err == nil {
			bridgeID = b.ID
		}
	}
	action := r.URL.Query().Get("action")
	entries, err := s.Store.AuditEntries(bridgeID, action, 200)
	if err != nil {
		httpError(w, 500, "audit: %v", err)
		return
	}
	bridges, _ := s.Store.Bridges()
	names := map[int64]string{}
	for _, b := range bridges {
		names[b.ID] = b.Slug
	}
	s.renderPage(w, r, "audit.html", map[string]any{
		"Entries": entries, "Names": names, "Filter": bridgeSlug, "ActionFilter": action, "Bridges": bridges,
	})
}
