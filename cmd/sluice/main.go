// Command sluice runs the Sluice server: web UI, webhook endpoint, job
// workers and cron scheduler in a single binary.
//
// Configuration (environment):
//
//	SLUICE_DATA_DIR        state directory (default ./data): sluice.db + workspaces/
//	SLUICE_LISTEN          listen address (default :8080)
//	SLUICE_ADMIN_PASSWORD  admin login password (required)
//	SLUICE_SECRET_KEY      64 hex chars; encrypts tokens/secrets at rest (required)
//	SLUICE_KNOWN_HOSTS     pinned known_hosts file for SSH remotes (default
//	                       $SLUICE_DATA_DIR/known_hosts if it exists)
//	SLUICE_WORKERS         job worker pool size (default 4)
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/geertarien/sluice/internal/jobs"
	"github.com/geertarien/sluice/internal/secrets"
	"github.com/geertarien/sluice/internal/store"
	"github.com/geertarien/sluice/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// checkPrereqs validates at startup that git >= 2.32 and git-filter-repo
// are available (spec §3).
func checkPrereqs() error {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return fmt.Errorf("git not found on PATH: %w", err)
	}
	m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		return fmt.Errorf("cannot parse git version from %q", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || (major == 2 && minor < 32) {
		return fmt.Errorf("git >= 2.32 required, found %s", strings.TrimSpace(string(out)))
	}
	if err := exec.Command("git", "filter-repo", "--version").Run(); err != nil {
		return fmt.Errorf("git-filter-repo not found on PATH (pip install git-filter-repo): %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if err := checkPrereqs(); err != nil {
		return err
	}
	dataDir := env("SLUICE_DATA_DIR", "data")
	workdir := filepath.Join(dataDir, "workspaces")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}

	adminPassword := os.Getenv("SLUICE_ADMIN_PASSWORD")
	if adminPassword == "" {
		return fmt.Errorf("SLUICE_ADMIN_PASSWORD must be set")
	}
	box, err := secrets.New(os.Getenv("SLUICE_SECRET_KEY"))
	if err != nil {
		return fmt.Errorf("SLUICE_SECRET_KEY: %w", err)
	}

	knownHosts := os.Getenv("SLUICE_KNOWN_HOSTS")
	if knownHosts == "" {
		if def := filepath.Join(dataDir, "known_hosts"); fileExists(def) {
			knownHosts = def
		}
	}
	if knownHosts == "" {
		log.Println("WARNING: no SLUICE_KNOWN_HOSTS configured; SSH host keys fall back to the system/user known_hosts")
	}

	st, err := store.Open(filepath.Join(dataDir, "sluice.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if err := st.ResetRunningJobs(); err != nil {
		return err
	}

	workers, _ := strconv.Atoi(env("SLUICE_WORKERS", "4"))
	svc := jobs.New(st, box, workdir, knownHosts, workers)

	srv, err := web.NewServer(st, box, svc, adminPassword)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc.Start(ctx)

	addr := env("SLUICE_LISTEN", ":8080")
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}()
	log.Printf("sluice listening on %s (data: %s, workers: %d)", addr, dataDir, workers)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
