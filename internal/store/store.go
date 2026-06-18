// Package store is the SQLite persistence layer: bridges, jobs, promotions
// and the audit log (spec §4).
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// SQLite handles one writer at a time; a single connection avoids
	// SQLITE_BUSY churn from concurrent job-log appends.
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.DB.Exec(`
CREATE TABLE IF NOT EXISTS bridges (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  slug TEXT NOT NULL UNIQUE,
  source_remote_url TEXT NOT NULL,
  gitea_base_url TEXT NOT NULL,
  gitea_owner TEXT NOT NULL,
  gitea_repo TEXT NOT NULL,
  gitea_ssh_url TEXT NOT NULL,
  gitea_token_enc BLOB,
  excluded_paths TEXT NOT NULL DEFAULT '[]',
  sync_branches TEXT NOT NULL DEFAULT '[]',
  sync_globs TEXT NOT NULL DEFAULT '[]',
  tripwire_strings TEXT NOT NULL DEFAULT '[]',
  promote_name TEXT NOT NULL DEFAULT '',
  promote_email TEXT NOT NULL DEFAULT '',
  promote_keep_trailer INTEGER NOT NULL DEFAULT 1,
  promote_signoff INTEGER NOT NULL DEFAULT 0,
  schedule_cron TEXT NOT NULL DEFAULT '',
  webhook_secret_enc BLOB,
  ssh_private_key_enc BLOB,
  ssh_public_key TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'paused',
  last_sync_at TIMESTAMP,
  last_sync_ok INTEGER,
  last_verified_at TIMESTAMP,
  last_verify_ok INTEGER,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  bridge_id INTEGER NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  payload TEXT NOT NULL DEFAULT '{}',
  log TEXT NOT NULL DEFAULT '',
  error_summary TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  started_at TIMESTAMP,
  finished_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS jobs_bridge ON jobs(bridge_id, status);
CREATE TABLE IF NOT EXISTS promotions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  bridge_id INTEGER NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  gitea_branch TEXT NOT NULL,
  gitea_pr_number INTEGER,
  real_branch TEXT NOT NULL,
  real_tip_sha TEXT NOT NULL DEFAULT '',
  base_branch TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  finalized_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  bridge_id INTEGER,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  details TEXT NOT NULL DEFAULT '{}',
  at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`)
	if err != nil {
		return err
	}
	// Upgrades: add columns introduced after the initial schema. CREATE TABLE
	// above already includes them for fresh databases; these are no-ops there.
	for _, c := range []struct{ col, def string }{
		{"ssh_private_key_enc", "BLOB"},
		{"ssh_public_key", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.addColumnIfMissing("bridges", c.col, c.def); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing adds a column to a table when it isn't already present,
// so existing databases pick up schema additions. table/column/def are
// in-code constants, never user input.
func (s *Store) addColumnIfMissing(table, column, def string) error {
	rows, err := s.DB.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.DB.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + def)
	return err
}

// ---------- bridges ----------

type Bridge struct {
	ID                 int64
	Name               string
	Slug               string
	SourceRemoteURL    string
	GiteaBaseURL       string
	GiteaOwner         string
	GiteaRepo          string
	GiteaSSHURL        string
	GiteaTokenEnc      []byte
	ExcludedPaths      []string
	SyncBranches       []string
	SyncGlobs          []string
	TripwireStrings    []string
	PromoteName        string
	PromoteEmail       string
	PromoteKeepTrailer bool
	PromoteSignoff     bool
	ScheduleCron       string
	WebhookSecretEnc   []byte
	SSHPrivateKeyEnc   []byte
	SSHPublicKey       string
	Status             string
	LastSyncAt         *time.Time
	LastSyncOK         *bool
	LastVerifiedAt     *time.Time
	LastVerifyOK       *bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func jsonArr(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func fromJSONArr(s string) []string {
	var v []string
	_ = json.Unmarshal([]byte(s), &v)
	return v
}

const bridgeCols = `id, name, slug, source_remote_url, gitea_base_url, gitea_owner,
 gitea_repo, gitea_ssh_url, gitea_token_enc, excluded_paths, sync_branches,
 sync_globs, tripwire_strings, promote_name, promote_email, promote_keep_trailer,
 promote_signoff, schedule_cron, webhook_secret_enc, ssh_private_key_enc,
 ssh_public_key, status,
 last_sync_at, last_sync_ok, last_verified_at, last_verify_ok, created_at, updated_at`

func scanBridge(row interface{ Scan(...any) error }) (*Bridge, error) {
	b := &Bridge{}
	var excl, branches, globs, tripwires string
	var lastSyncOK, lastVerifyOK sql.NullBool
	var lastSyncAt, lastVerifiedAt sql.NullTime
	err := row.Scan(&b.ID, &b.Name, &b.Slug, &b.SourceRemoteURL, &b.GiteaBaseURL,
		&b.GiteaOwner, &b.GiteaRepo, &b.GiteaSSHURL, &b.GiteaTokenEnc,
		&excl, &branches, &globs, &tripwires,
		&b.PromoteName, &b.PromoteEmail, &b.PromoteKeepTrailer, &b.PromoteSignoff,
		&b.ScheduleCron, &b.WebhookSecretEnc, &b.SSHPrivateKeyEnc, &b.SSHPublicKey, &b.Status,
		&lastSyncAt, &lastSyncOK, &lastVerifiedAt, &lastVerifyOK,
		&b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.ExcludedPaths = fromJSONArr(excl)
	b.SyncBranches = fromJSONArr(branches)
	b.SyncGlobs = fromJSONArr(globs)
	b.TripwireStrings = fromJSONArr(tripwires)
	if lastSyncAt.Valid {
		b.LastSyncAt = &lastSyncAt.Time
	}
	if lastSyncOK.Valid {
		b.LastSyncOK = &lastSyncOK.Bool
	}
	if lastVerifiedAt.Valid {
		b.LastVerifiedAt = &lastVerifiedAt.Time
	}
	if lastVerifyOK.Valid {
		b.LastVerifyOK = &lastVerifyOK.Bool
	}
	return b, nil
}

func (s *Store) CreateBridge(b *Bridge) error {
	res, err := s.DB.Exec(`INSERT INTO bridges
 (name, slug, source_remote_url, gitea_base_url, gitea_owner, gitea_repo,
  gitea_ssh_url, gitea_token_enc, excluded_paths, sync_branches, sync_globs,
  tripwire_strings, promote_name, promote_email, promote_keep_trailer,
  promote_signoff, schedule_cron, webhook_secret_enc, ssh_private_key_enc,
  ssh_public_key, status)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.Name, b.Slug, b.SourceRemoteURL, b.GiteaBaseURL, b.GiteaOwner, b.GiteaRepo,
		b.GiteaSSHURL, b.GiteaTokenEnc, jsonArr(b.ExcludedPaths), jsonArr(b.SyncBranches),
		jsonArr(b.SyncGlobs), jsonArr(b.TripwireStrings), b.PromoteName, b.PromoteEmail,
		b.PromoteKeepTrailer, b.PromoteSignoff, b.ScheduleCron, b.WebhookSecretEnc,
		b.SSHPrivateKeyEnc, b.SSHPublicKey, b.Status)
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateBridge(b *Bridge) error {
	_, err := s.DB.Exec(`UPDATE bridges SET
 name=?, source_remote_url=?, gitea_base_url=?, gitea_owner=?, gitea_repo=?,
 gitea_ssh_url=?, gitea_token_enc=?, excluded_paths=?, sync_branches=?,
 sync_globs=?, tripwire_strings=?, promote_name=?, promote_email=?,
 promote_keep_trailer=?, promote_signoff=?, schedule_cron=?,
 webhook_secret_enc=?, ssh_private_key_enc=?, ssh_public_key=?,
 status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		b.Name, b.SourceRemoteURL, b.GiteaBaseURL, b.GiteaOwner, b.GiteaRepo,
		b.GiteaSSHURL, b.GiteaTokenEnc, jsonArr(b.ExcludedPaths), jsonArr(b.SyncBranches),
		jsonArr(b.SyncGlobs), jsonArr(b.TripwireStrings), b.PromoteName, b.PromoteEmail,
		b.PromoteKeepTrailer, b.PromoteSignoff, b.ScheduleCron,
		b.WebhookSecretEnc, b.SSHPrivateKeyEnc, b.SSHPublicKey, b.Status, b.ID)
	return err
}

func (s *Store) SetBridgeStatus(id int64, status string) error {
	_, err := s.DB.Exec(`UPDATE bridges SET status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, id)
	return err
}

func (s *Store) SetBridgeSyncResult(id int64, ok bool) error {
	_, err := s.DB.Exec(`UPDATE bridges SET last_sync_at=CURRENT_TIMESTAMP, last_sync_ok=? WHERE id=?`, ok, id)
	return err
}

func (s *Store) SetBridgeVerifyResult(id int64, ok bool) error {
	_, err := s.DB.Exec(`UPDATE bridges SET last_verified_at=CURRENT_TIMESTAMP, last_verify_ok=? WHERE id=?`, ok, id)
	return err
}

func (s *Store) BridgeBySlug(slug string) (*Bridge, error) {
	return scanBridge(s.DB.QueryRow(`SELECT `+bridgeCols+` FROM bridges WHERE slug=?`, slug))
}

func (s *Store) BridgeByID(id int64) (*Bridge, error) {
	return scanBridge(s.DB.QueryRow(`SELECT `+bridgeCols+` FROM bridges WHERE id=?`, id))
}

func (s *Store) Bridges() ([]*Bridge, error) {
	rows, err := s.DB.Query(`SELECT ` + bridgeCols + ` FROM bridges ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Bridge
	for rows.Next() {
		b, err := scanBridge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBridge(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM bridges WHERE id=?`, id)
	return err
}

// ---------- jobs ----------

type Job struct {
	ID           int64
	BridgeID     int64
	Kind         string // sync|promote|finalize|init|verify
	Status       string // queued|running|success|failed|needs_attention
	Payload      string // JSON
	Log          string
	ErrorSummary string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

func (s *Store) EnqueueJob(bridgeID int64, kind string, payload any) (int64, error) {
	pl := "{}"
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		pl = string(b)
	}
	res, err := s.DB.Exec(`INSERT INTO jobs (bridge_id, kind, payload) VALUES (?,?,?)`,
		bridgeID, kind, pl)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ClaimJob atomically picks the oldest queued job whose bridge has no
// running job (per-bridge mutex, spec §5.3) and marks it running.
func (s *Store) ClaimJob() (*Job, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT id FROM jobs WHERE status='queued' AND bridge_id NOT IN
 (SELECT bridge_id FROM jobs WHERE status='running') ORDER BY id LIMIT 1`)
	var id int64
	if err := row.Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET status='running', started_at=CURRENT_TIMESTAMP WHERE id=?`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.JobByID(id)
}

func (s *Store) AppendJobLog(id int64, text string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET log = log || ? WHERE id=?`, text, id)
	return err
}

func (s *Store) FinishJob(id int64, status, errorSummary string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET status=?, error_summary=?, finished_at=CURRENT_TIMESTAMP WHERE id=?`,
		status, errorSummary, id)
	return err
}

// ResetRunningJobs marks jobs left 'running' by a crash as failed (startup).
func (s *Store) ResetRunningJobs() error {
	_, err := s.DB.Exec(`UPDATE jobs SET status='failed',
 error_summary='interrupted by restart', finished_at=CURRENT_TIMESTAMP WHERE status='running'`)
	return err
}

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	j := &Job{}
	var started, finished sql.NullTime
	err := row.Scan(&j.ID, &j.BridgeID, &j.Kind, &j.Status, &j.Payload, &j.Log,
		&j.ErrorSummary, &j.CreatedAt, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if started.Valid {
		j.StartedAt = &started.Time
	}
	if finished.Valid {
		j.FinishedAt = &finished.Time
	}
	return j, nil
}

const jobCols = `id, bridge_id, kind, status, payload, log, error_summary, created_at, started_at, finished_at`

func (s *Store) JobByID(id int64) (*Job, error) {
	return scanJob(s.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (s *Store) JobsForBridge(bridgeID int64, limit int) ([]*Job, error) {
	rows, err := s.DB.Query(`SELECT `+jobCols+` FROM jobs WHERE bridge_id=? ORDER BY id DESC LIMIT ?`, bridgeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJobs(rows)
}

func (s *Store) JobsNeedingAttention(bridgeID int64) ([]*Job, error) {
	rows, err := s.DB.Query(`SELECT `+jobCols+` FROM jobs WHERE bridge_id=? AND status='needs_attention' ORDER BY id DESC`, bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJobs(rows)
}

func collectJobs(rows *sql.Rows) ([]*Job, error) {
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) CountJobs(bridgeID int64, status string) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE bridge_id=? AND status=?`, bridgeID, status).Scan(&n)
	return n, err
}

// ---------- promotions ----------

type Promotion struct {
	ID            int64
	BridgeID      int64
	GiteaBranch   string
	GiteaPRNumber *int64
	RealBranch    string
	RealTipSHA    string
	BaseBranch    string
	Status        string // promoted|landed|finalized|conflict|rejected|needs_attention
	CreatedAt     time.Time
	FinalizedAt   *time.Time
}

func (s *Store) CreatePromotion(p *Promotion) error {
	res, err := s.DB.Exec(`INSERT INTO promotions
 (bridge_id, gitea_branch, gitea_pr_number, real_branch, real_tip_sha, base_branch, status)
 VALUES (?,?,?,?,?,?,?)`,
		p.BridgeID, p.GiteaBranch, p.GiteaPRNumber, p.RealBranch, p.RealTipSHA, p.BaseBranch, p.Status)
	if err != nil {
		return err
	}
	p.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdatePromotionStatus(id int64, status string) error {
	var err error
	if status == "finalized" {
		_, err = s.DB.Exec(`UPDATE promotions SET status=?, finalized_at=CURRENT_TIMESTAMP WHERE id=?`, status, id)
	} else {
		_, err = s.DB.Exec(`UPDATE promotions SET status=? WHERE id=?`, status, id)
	}
	return err
}

func (s *Store) SetPromotionTip(id int64, sha string) error {
	_, err := s.DB.Exec(`UPDATE promotions SET real_tip_sha=? WHERE id=?`, sha, id)
	return err
}

func (s *Store) MarkOpenPromotionsNeedsAttention(bridgeID int64) error {
	_, err := s.DB.Exec(`UPDATE promotions SET status='needs_attention'
 WHERE bridge_id=? AND status IN ('promoted','conflict')`, bridgeID)
	return err
}

func scanPromotion(row interface{ Scan(...any) error }) (*Promotion, error) {
	p := &Promotion{}
	var pr sql.NullInt64
	var fin sql.NullTime
	err := row.Scan(&p.ID, &p.BridgeID, &p.GiteaBranch, &pr, &p.RealBranch,
		&p.RealTipSHA, &p.BaseBranch, &p.Status, &p.CreatedAt, &fin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if pr.Valid {
		p.GiteaPRNumber = &pr.Int64
	}
	if fin.Valid {
		p.FinalizedAt = &fin.Time
	}
	return p, nil
}

const promoCols = `id, bridge_id, gitea_branch, gitea_pr_number, real_branch, real_tip_sha, base_branch, status, created_at, finalized_at`

func (s *Store) PromotionByID(id int64) (*Promotion, error) {
	return scanPromotion(s.DB.QueryRow(`SELECT `+promoCols+` FROM promotions WHERE id=?`, id))
}

func (s *Store) PromotionsForBridge(bridgeID int64) ([]*Promotion, error) {
	rows, err := s.DB.Query(`SELECT `+promoCols+` FROM promotions WHERE bridge_id=? ORDER BY id DESC`, bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Promotion
	for rows.Next() {
		p, err := scanPromotion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PromotionsByStatus(bridgeID int64, status string) ([]*Promotion, error) {
	rows, err := s.DB.Query(`SELECT `+promoCols+` FROM promotions WHERE bridge_id=? AND status=? ORDER BY id`, bridgeID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Promotion
	for rows.Next() {
		p, err := scanPromotion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- audit ----------

type AuditEntry struct {
	ID       int64
	BridgeID *int64
	Actor    string
	Action   string
	Details  string
	At       time.Time
}

func (s *Store) Audit(bridgeID int64, actor, action string, details any) {
	d := "{}"
	if details != nil {
		if b, err := json.Marshal(details); err == nil {
			d = string(b)
		}
	}
	var bid any
	if bridgeID != 0 {
		bid = bridgeID
	}
	_, _ = s.DB.Exec(`INSERT INTO audit_log (bridge_id, actor, action, details) VALUES (?,?,?,?)`,
		bid, actor, action, d)
}

func (s *Store) AuditEntries(bridgeID int64, action string, limit int) ([]*AuditEntry, error) {
	q := `SELECT id, bridge_id, actor, action, details, at FROM audit_log WHERE 1=1`
	args := []any{}
	if bridgeID != 0 {
		q += ` AND bridge_id=?`
		args = append(args, bridgeID)
	}
	if action != "" {
		q += ` AND action=?`
		args = append(args, action)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		e := &AuditEntry{}
		var bid sql.NullInt64
		if err := rows.Scan(&e.ID, &bid, &e.Actor, &e.Action, &e.Details, &e.At); err != nil {
			return nil, err
		}
		if bid.Valid {
			e.BridgeID = &bid.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DashboardCounts returns per-bridge counts used by the home page.
func (s *Store) DashboardCounts(bridgeID int64) (awaitingMerge, needsAttention int, err error) {
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM promotions WHERE bridge_id=? AND status='promoted'`, bridgeID).Scan(&awaitingMerge); err != nil {
		return
	}
	err = s.DB.QueryRow(`SELECT
 (SELECT COUNT(*) FROM jobs WHERE bridge_id=? AND status='needs_attention') +
 (SELECT COUNT(*) FROM promotions WHERE bridge_id=? AND status IN ('conflict','needs_attention'))`,
		bridgeID, bridgeID).Scan(&needsAttention)
	return
}
