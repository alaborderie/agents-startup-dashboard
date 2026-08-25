package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alaborderie/agents-startup-dashboard/internal/store"
)

type stubStore struct {
	ventures []store.DashboardVenture
	lastSync *time.Time
	result   store.ApplyResult
}

func (s *stubStore) ApplySnapshot(_ context.Context, _ store.Snapshot, _ []store.Venture, _ []string, _ []string) (store.ApplyResult, error) {
	return s.result, nil
}

func (s *stubStore) ListVentures(context.Context) ([]store.DashboardVenture, error) {
	return s.ventures, nil
}

func (s *stubStore) LastSync(context.Context) (*time.Time, error) {
	return s.lastSync, nil
}

func TestDashboardEscapesProseAndAppliesPublicationRules(t *testing.T) {
	syncedAt := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	dataStore := &stubStore{
		lastSync: &syncedAt,
		ventures: []store.DashboardVenture{
			{
				Title: "Unsafe idea", Field: "development", Status: "abandoned",
				Kind: "internal", Description: "<script>alert(1)</script>",
				Monetization: "must not render", RepoURL: "https://example.invalid/private",
			},
		},
	}
	app := newTestApp(t, dataStore)
	app.now = func() time.Time { return syncedAt.Add(12 * time.Minute) }

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	body := response.Body.String()

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("unsafe description was not rendered as escaped text: %s", body)
	}
	for _, forbidden := range []string{"must not render", "https://example.invalid/private"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("page contains forbidden value %q", forbidden)
		}
	}
	for _, required := range []string{"reason not recorded", "last sync: 12 minutes ago"} {
		if !strings.Contains(body, required) {
			t.Fatalf("page missing %q", required)
		}
	}
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("X-Robots-Tag"); got != "noindex" {
		t.Fatalf("X-Robots-Tag = %q", got)
	}
}

func TestDashboardEmptyStateWithNullableFields(t *testing.T) {
	dataStore := &stubStore{ventures: []store.DashboardVenture{
		{Title: "Minimal", Field: "other", Status: "in_progress"},
	}}
	app := newTestApp(t, dataStore)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "last sync: never") {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}

	empty := newTestApp(t, &stubStore{})
	response = httptest.NewRecorder()
	empty.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "No ventures have arrived yet") {
		t.Fatalf("empty state: status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestDashboardShowsDesignReviewStatusAndCanvasLink(t *testing.T) {
	designURL := "https://design.example.invalid/canvas/venture-1"
	dataStore := &stubStore{ventures: []store.DashboardVenture{
		{
			Slug: "venture-1", Title: "Reviewable design", Field: "development",
			Status: "in_progress", Stage: "DESIGN_REVIEW", DesignURL: designURL,
		},
	}}
	app := newTestApp(t, dataStore)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, body)
	}
	for _, required := range []string{"DESIGN_REVIEW", `href="` + designURL + `"`, "Review design"} {
		if !strings.Contains(body, required) {
			t.Fatalf("page missing %q: %s", required, body)
		}
	}
}

func TestRobotsAndHealth(t *testing.T) {
	app := newTestApp(t, &stubStore{})
	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/healthz", body: "ok\n"},
		{path: "/robots.txt", body: "User-agent: *\nDisallow: /\n"},
	} {
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK || response.Body.String() != test.body {
			t.Fatalf("%s: status = %d body = %q", test.path, response.Code, response.Body.String())
		}
	}
}

func TestIngestRejectsBadAuthWithEmptyBody(t *testing.T) {
	dataStore := &stubStore{}
	app := newTestApp(t, dataStore)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader("{}")))
	if response.Code != http.StatusUnauthorized || response.Body.Len() != 0 {
		t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
	}
}

func TestIngestRejectsOversizedBody(t *testing.T) {
	app := newTestApp(t, &stubStore{})
	body := `{"padding":"` + strings.Repeat("x", maxRequestBody) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/ingest", io.LimitReader(strings.NewReader(body), maxRequestBody+100))
	request.Header.Set("Authorization", "Bearer fake-test-token")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
	}
}

func TestVentureValidationIsLenientAndTruncatesUnicode(t *testing.T) {
	longDescription := strings.Repeat("é", 601)
	missingReason := "  "
	ventures := []store.Venture{
		{Slug: "valid", Title: "Valid", Field: "development", Status: "abandoned", Description: &longDescription, AbandonedReason: &missingReason},
		{Slug: "bad-field", Title: "Bad", Field: "not-real", Status: "deployed"},
		{Slug: "bad-status", Title: "Bad", Field: "other", Status: "paused"},
		{Slug: "", Title: "No slug", Field: "other", Status: "deployed"},
	}
	valid, present, warnings := validateVentures(ventures)
	if len(valid) != 1 || len([]rune(*valid[0].Description)) != 600 {
		t.Fatalf("valid ventures = %#v", valid)
	}
	if len(present) != 3 {
		t.Fatalf("present slugs = %#v", present)
	}
	if len(warnings) != 4 {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestDesignReviewValidationKeepsRowAndReportsBadOrMissingURL(t *testing.T) {
	stage := "DESIGN_REVIEW"
	badURL := "javascript:alert(1)"
	ventures := []store.Venture{
		{Slug: "missing", Title: "Missing", Field: "development", Status: "in_progress", Stage: &stage},
		{Slug: "unsafe", Title: "Unsafe", Field: "development", Status: "in_progress", Stage: &stage, DesignURL: &badURL},
	}
	valid, _, warnings := validateVentures(ventures)
	if len(valid) != 2 {
		t.Fatalf("valid ventures = %#v", valid)
	}
	if valid[1].DesignURL != nil {
		t.Fatalf("unsafe design URL survived validation: %#v", valid[1].DesignURL)
	}
	if len(warnings) != 3 {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func newTestApp(t *testing.T, dataStore DataStore) *App {
	t.Helper()
	app, err := New(dataStore, "fake-test-token")
	if err != nil {
		t.Fatal(err)
	}
	return app
}
