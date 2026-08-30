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

func TestDashboardShowsDesignReviewInsteadOfRawStatus(t *testing.T) {
	designURL := "https://design.example.invalid/canvas/venture-1"
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Slug: "venture-1", Title: "Reviewable design", Field: "development",
			Status: "in_progress", Stage: "DESIGN_REVIEW", DesignURL: designURL,
		},
	}}).Body.String()

	if !strings.Contains(body, `class="status status-design_review"`) ||
		!strings.Contains(body, "DESIGN_REVIEW") {
		t.Fatalf("page missing design-review status: %s", body)
	}
	if strings.Contains(body, `class="status status-in_progress"`) {
		t.Fatalf("page rendered raw status as the status chip: %s", body)
	}
}

func TestDashboardShowsDesignCanvasLink(t *testing.T) {
	designURL := "https://design.example.invalid/canvas/venture-1"
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Slug: "venture-1", Title: "Reviewable design", Field: "development",
			Status: "in_progress", Stage: "DESIGN_REVIEW", DesignURL: designURL,
		},
	}}).Body.String()

	if !strings.Contains(body, `href="`+designURL+`"`) || !strings.Contains(body, "Review design") {
		t.Fatalf("page missing design canvas link: %s", body)
	}
}

func TestDashboardShowsMissingAbandonedReason(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{Title: "Missing reason", Field: "development", Status: "abandoned"},
	}}).Body.String()

	if !strings.Contains(body, `class="missing-reason"`) ||
		!strings.Contains(body, "reason not recorded") {
		t.Fatalf("page missing exact absent-reason marker: %s", body)
	}
}

func TestDashboardShowsRecordedAbandonedReason(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Title: "Recorded reason", Field: "development", Status: "abandoned",
			AbandonedReason: `<same user & channel>`,
		},
	}}).Body.String()

	if !strings.Contains(body, "&lt;same user &amp; channel&gt;") {
		t.Fatalf("page missing escaped abandoned reason: %s", body)
	}
	if strings.Contains(body, "reason not recorded") {
		t.Fatalf("page rendered absent-reason marker for a recorded reason: %s", body)
	}
}

func TestDashboardShowsAbandonedStage(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Title: "Screened venture", Field: "development", Status: "abandoned",
			AbandonedAtStage: "SCREENED",
		},
	}}).Body.String()

	if !strings.Contains(body, `class="reason-stage"`) ||
		!strings.Contains(body, "Killed at SCREENED") {
		t.Fatalf("page missing abandoned-stage label: %s", body)
	}
}

func TestDashboardSuppressesInternalMonetization(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Title: "Internal venture", Field: "development", Status: "in_progress",
			Kind: "internal", Monetization: "private plan",
		},
	}}).Body.String()

	if strings.Contains(body, "Monetization") || strings.Contains(body, "private plan") {
		t.Fatalf("page rendered monetization for an internal venture: %s", body)
	}
}

func TestDashboardShowsProductMonetization(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Title: "Product venture", Field: "development", Status: "in_progress",
			Kind: "product", Monetization: "Per project",
		},
	}}).Body.String()

	if !strings.Contains(body, `class="monetization"`) ||
		!strings.Contains(body, "Monetization:") || !strings.Contains(body, "Per project") {
		t.Fatalf("page missing product monetization: %s", body)
	}
}

func TestDashboardShowsLiveURLAndSuppressesAbandonedRepoURL(t *testing.T) {
	liveURL := "https://live.example.invalid"
	repoURL := "https://repo.example.invalid/private"
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Title: "Abandoned venture", Field: "development", Status: "abandoned",
			URL: liveURL, RepoURL: repoURL,
		},
	}}).Body.String()

	if !strings.Contains(body, `href="`+liveURL+`"`) {
		t.Fatalf("page missing venture URL: %s", body)
	}
	if strings.Contains(body, repoURL) {
		t.Fatalf("page rendered repository URL for an abandoned venture: %s", body)
	}
}

func TestDashboardEscapesProse(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{
			Title: "Unsafe idea", Field: "development", Status: "in_progress",
			Description: "<script>alert(1)</script>",
		},
	}}).Body.String()

	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("unsafe description was not rendered as escaped text: %s", body)
	}
}

func TestDashboardShowsEmptyVentureList(t *testing.T) {
	body := renderDashboard(t, &stubStore{}).Body.String()

	if !strings.Contains(body, "No ventures have arrived yet.") {
		t.Fatalf("page missing empty-list state: %s", body)
	}
}

func TestDashboardShowsNeverSynced(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{Title: "Minimal", Field: "other", Status: "in_progress"},
	}}).Body.String()

	if !strings.Contains(body, "last sync: never") {
		t.Fatalf("page missing never-synced state: %s", body)
	}
}

func TestDashboardResponseMetadata(t *testing.T) {
	syncedAt := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	app := newTestApp(t, &stubStore{lastSync: &syncedAt})
	app.now = func() time.Time { return syncedAt.Add(12 * time.Minute) }
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "last sync: 12 minutes ago") {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("X-Robots-Tag"); got != "noindex" {
		t.Fatalf("X-Robots-Tag = %q", got)
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

func renderDashboard(t *testing.T, dataStore DataStore) *httptest.ResponseRecorder {
	t.Helper()
	app := newTestApp(t, dataStore)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	return response
}

func newTestApp(t *testing.T, dataStore DataStore) *App {
	t.Helper()
	app, err := New(dataStore, "fake-test-token")
	if err != nil {
		t.Fatal(err)
	}
	return app
}
