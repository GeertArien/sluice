package gitea

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestOpenPRCountUsesIssuesEndpoint checks the count comes from the fast
// issues?type=pulls endpoint via X-Total-Count, not the expensive pulls list.
func TestOpenPRCountUsesIssuesEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		if strings.Contains(r.URL.Path, "/pulls") {
			t.Errorf("count must not hit the pulls list: %s", r.URL.Path)
		}
		w.Header().Set("X-Total-Count", "7")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	n, err := New(srv.URL, "tok").OpenPRCount(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("count = %d, want 7 (from X-Total-Count)", n)
	}
	if !strings.Contains(gotPath, "/repos/o/r/issues") || !strings.Contains(gotPath, "type=pulls") {
		t.Fatalf("unexpected endpoint: %s", gotPath)
	}
}

// TestOpenPRCountFallsBackToPageLen covers a forge that omits X-Total-Count.
func TestOpenPRCountFallsBackToPageLen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"number":1},{"number":2},{"number":3}]`))
	}))
	defer srv.Close()

	n, err := New(srv.URL, "tok").OpenPRCount(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3 (page length fallback)", n)
	}
}

// TestFindOpenPRByHeadUsesBaseHead asserts the direct /pulls/{base}/{head}
// lookup is used when the base is known, and the list is never scanned.
func TestFindOpenPRByHeadUsesBaseHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls") {
			t.Errorf("must not scan the pulls list when base is known: %s", r.URL.RawQuery)
		}
		if r.URL.Path == "/api/v1/repos/o/r/pulls/main/feat" {
			fmt.Fprint(w, `{"number":42,"state":"open","head":{"ref":"feat"},"base":{"ref":"main"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	pr, err := New(srv.URL, "tok").FindOpenPRByHead(context.Background(), "o", "r", "main", "feat")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 42 {
		t.Fatalf("expected PR #42 via base/head lookup, got %+v", pr)
	}
}

// TestFindOpenPRByHeadBaseHead404 returns nil (no PR), not an error.
func TestFindOpenPRByHeadBaseHead404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	pr, err := New(srv.URL, "tok").FindOpenPRByHead(context.Background(), "o", "r", "main", "feat")
	if err != nil {
		t.Fatalf("404 should mean no PR, not error: %v", err)
	}
	if pr != nil {
		t.Fatalf("expected nil for absent base/head PR, got %+v", pr)
	}
}

// TestFindOpenPRByHeadPaginates covers the base-unknown path: the list is paged
// until the match is found, so a match past the first page of 50 isn't lost.
func TestFindOpenPRByHeadPaginates(t *testing.T) {
	var pages int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls") {
			t.Errorf("base-unknown lookup must page the list, hit %s", r.URL.Path)
		}
		atomic.AddInt32(&pages, 1)
		switch r.URL.Query().Get("page") {
		case "1":
			// A full page of 50 non-matching PRs → the client must fetch page 2.
			var b strings.Builder
			b.WriteByte('[')
			for i := 0; i < 50; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"number":%d,"state":"open","head":{"ref":"other-%d"}}`, i, i)
			}
			b.WriteByte(']')
			_, _ = w.Write([]byte(b.String()))
		case "2":
			fmt.Fprint(w, `[{"number":99,"state":"open","head":{"ref":"wanted"}}]`)
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	defer srv.Close()

	// Empty base forces the paged path.
	pr, err := New(srv.URL, "tok").FindOpenPRByHead(context.Background(), "o", "r", "", "wanted")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 99 {
		t.Fatalf("expected PR #99 from page 2, got %+v", pr)
	}
	if atomic.LoadInt32(&pages) < 2 {
		t.Fatalf("expected pagination beyond page 1, saw %d page fetches", pages)
	}
}

// TestFindOpenPRByHeadSlashBaseFallsBackToPaging: a base/head containing a
// slash can't use the path-param endpoint, so it must page instead.
func TestFindOpenPRByHeadSlashBaseFallsBackToPaging(t *testing.T) {
	var listed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls") {
			listed = true
			fmt.Fprint(w, `[{"number":7,"state":"open","head":{"ref":"feature/x"}}]`)
			return
		}
		t.Errorf("must not use base/head path lookup for slashed refs: %s", r.URL.Path)
	}))
	defer srv.Close()

	pr, err := New(srv.URL, "tok").FindOpenPRByHead(context.Background(), "o", "r", "release/4.6", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 7 {
		t.Fatalf("expected PR #7 via paged fallback, got %+v", pr)
	}
	if !listed {
		t.Fatal("expected the paged list to be used for slashed refs")
	}
}

func TestVersionParsesForgeString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"version":"15.0.7+gitea-1.22.0"}`)
	}))
	defer srv.Close()

	v, err := New(srv.URL, "tok").Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "15.0.7+gitea-1.22.0" {
		t.Fatalf("version = %q", v)
	}
}
