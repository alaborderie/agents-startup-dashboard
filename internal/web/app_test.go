package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alaborderie/agents-startup-dashboard/internal/store"
)

type stubStore struct {
	ventures []store.DashboardVenture
	spend    store.DashboardSpend
	lastSync *time.Time
	result   store.ApplyResult
}

func (s *stubStore) ApplySnapshot(_ context.Context, _ store.Snapshot, _ []store.Venture, _ []string, _ []string) (store.ApplyResult, error) {
	return s.result, nil
}

func (s *stubStore) ListVentures(context.Context) ([]store.DashboardVenture, error) {
	return s.ventures, nil
}

func (s *stubStore) DashboardSpend(context.Context) (store.DashboardSpend, error) {
	return s.spend, nil
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

func TestDashboardShowsHonestNoSpendState(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{Title: "Unmeasured", Field: "development", Status: "in_progress"},
	}}).Body.String()

	if !strings.Contains(body, "No spend data received yet") {
		t.Fatalf("page missing no-spend state: %s", body)
	}
	if strings.Contains(body, `class="spend-grid"`) || strings.Contains(body, `class="venture-spend"`) {
		t.Fatalf("page rendered zero figures for absent spend: %s", body)
	}
}

func TestDashboardLabelsEveryTokenFigureAndRendersSpendStates(t *testing.T) {
	first := time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC)
	last := time.Date(2026, 8, 30, 18, 1, 42, 0, time.UTC)
	company := &store.CompanyTokens{
		TokenAggregate: store.TokenAggregate{
			Runs: 3, BackfilledRuns: 2, Total: 4_000_000, Fresh: 3_000_000,
			CostUSD: 1.25, CostMeasuredRuns: 1,
		},
		LiveRuns: 1, UnattributedRuns: 1, UnattributedTotal: 1_500_000, UnattributedFresh: 1_000_000,
	}
	body := renderDashboard(t, &stubStore{
		ventures: []store.DashboardVenture{
			{
				Title: "Largest", Field: "development", Status: "in_progress",
				Tokens: &store.TokenAggregate{Runs: 2, BackfilledRuns: 1, Total: 2_500_000, Fresh: 2_000_000, CostUSD: .75, CostMeasuredRuns: 1},
			},
			{
				Title: "Half size", Field: "other", Status: "abandoned",
				Tokens: &store.TokenAggregate{Runs: 1, Total: 1_250_000, Fresh: 1_000_000},
			},
			{Title: "Predates ledger", Field: "other", Status: "abandoned"},
		},
		spend: store.DashboardSpend{
			Company: company, FirstTS: &first, LastTS: &last, Records: 3,
			Roles: []store.RoleTokens{
				{Role: "developer", TokenAggregate: store.TokenAggregate{Runs: 2, BackfilledRuns: 1, Fresh: 2_000_000}},
				{Role: "ceo", TokenAggregate: store.TokenAggregate{Runs: 1, Fresh: 1_000_000}},
			},
			Unmatched: []store.VentureTokens{{
				Slug: "orphan-ledger-row", TokenAggregate: store.TokenAggregate{Runs: 1, BackfilledRuns: 1, Fresh: 250_000},
			}},
		},
	}).Body.String()

	figurePattern := regexp.MustCompile(`<span class="token-figure"><span class="token-value">(?:<span class="approx" aria-label="approximately">≈</span>)?[^<]+</span> <span class="token-label">(?:fresh|fresh tokens|including cache reads)</span></span>`)
	if got, want := len(figurePattern.FindAllString(body, -1)), strings.Count(body, `class="token-figure"`); got != want || want == 0 {
		t.Fatalf("labelled token figures = %d of %d: %s", got, want, body)
	}
	for _, required := range []string{
		`class="spend-cell spend-cell-fresh"`, "fresh tokens", "including cache reads",
		"through 30 Aug 2026 18:01 UTC",
		"33.3%", "orchestration (CEO) — not attributable to any venture",
		"$1.25", "measured across 1 of 3 runs — opencode only; codex and Claude report no price",
		"orphan-ledger-row", "Spend by agent", ">ceo<",
		"≈ reconstructed by matching engine sessions", "orchestration at ~69% of all spend to date",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("page missing %q: %s", required, body)
		}
	}
	if got := strings.Count(body, `class="venture-spend"`); got != 2 {
		t.Fatalf("venture spend rows = %d, want only two measured ventures: %s", got, body)
	}
	if !strings.Contains(body, `style="width: 50.0%"`) || !strings.Contains(body, `style="width: 100.0%"`) {
		t.Fatalf("page missing proportional bar widths: %s", body)
	}
}

func TestTokenFormattingIsCompactAndLabelHelperOwnsMarkup(t *testing.T) {
	for value, want := range map[int64]string{
		999: "999", 1_000: "1k", 7_750_000: "7.75M", 45_500_000: "45.5M", 3_180_000_000: "3.18B",
	} {
		if got := formatTokenCount(value); got != want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", value, got, want)
		}
	}
	markup := string(renderTokenFigure(tokenFigure(12_000, "fresh", true)))
	if markup != `<span class="token-figure"><span class="token-value"><span class="approx" aria-label="approximately">≈</span>12k</span> <span class="token-label">fresh</span></span>` {
		t.Fatalf("token helper markup = %s", markup)
	}
}

func TestDashboardUsesOneCardLayoutAtEveryWidth(t *testing.T) {
	body := renderDashboard(t, &stubStore{ventures: []store.DashboardVenture{
		{Title: "First venture", Field: "development", Status: "in_progress"},
		{Title: "Second venture", Field: "other", Status: "deployed"},
	}}).Body.String()

	if got := strings.Count(body, `<article class="venture">`); got != 2 {
		t.Fatalf("venture card count = %d body = %s", got, body)
	}
	for _, obsolete := range []string{"<table", "<thead", "<tbody", "<tr", "<th", "<td", "data-label="} {
		if strings.Contains(body, obsolete) {
			t.Fatalf("page contains obsolete table markup %q: %s", obsolete, body)
		}
	}
}

func TestCardStylesKeepNarrowViewportsFromOverflowing(t *testing.T) {
	stylesheet, err := webFiles.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(stylesheet)
	for _, required := range []string{
		"grid-template-columns: minmax(0, 1fr)",
		"grid-template-columns: 200px minmax(0, 1fr)",
		"min-height: 44px",
		"overflow-wrap: anywhere",
		"a:focus-visible",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("stylesheet missing narrow-viewport guard %q", required)
		}
	}
	for _, obsolete := range []string{"white-space: nowrap", "table, tbody", "td::before"} {
		if strings.Contains(css, obsolete) {
			t.Errorf("stylesheet contains obsolete overflow hazard %q", obsolete)
		}
	}
}

func TestPageGutterUsesBroadlySupportedCalc(t *testing.T) {
	stylesheet, err := webFiles.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(stylesheet)
	if !strings.Contains(css, "calc(100% - var(--gutter) - var(--gutter))") {
		t.Fatal("stylesheet does not preserve page gutters with additive calc syntax")
	}
	if strings.Contains(css, "2 * var(--gutter)") {
		t.Fatal("stylesheet uses CSS typed multiplication for the page gutter")
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

func TestDashboardServesContentHashedStylesheet(t *testing.T) {
	app := newTestApp(t, &stubStore{})
	handler := app.Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	body := response.Body.String()
	if got := strings.Count(body, `<link rel="stylesheet"`); got != 1 {
		t.Fatalf("stylesheet link count = %d body = %s", got, body)
	}
	if strings.Contains(body, "<style") {
		t.Fatalf("page contains inline style element: %s", body)
	}
	match := regexp.MustCompile(`href="(/static/app\.[0-9a-f]{8,}\.css)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("page missing content-hashed stylesheet path: %s", body)
	}

	stylesheet := httptest.NewRecorder()
	handler.ServeHTTP(stylesheet, httptest.NewRequest(http.MethodGet, match[1], nil))
	if stylesheet.Code != http.StatusOK {
		t.Fatalf("stylesheet status = %d body = %s", stylesheet.Code, stylesheet.Body.String())
	}
	if got := stylesheet.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Fatalf("stylesheet Content-Type = %q", got)
	}
	if got := stylesheet.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("stylesheet Cache-Control = %q", got)
	}
	if !strings.Contains(stylesheet.Body.String(), ".status-abandoned") {
		t.Fatalf("stylesheet missing known selector: %s", stylesheet.Body.String())
	}
}

func TestDashboardRejectsUnknownStaticPaths(t *testing.T) {
	handler := newTestApp(t, &stubStore{}).Handler()
	for _, path := range []string{
		"/static/app.deadbeef.css",
		"/static/../app.go",
		"/static/app.css",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d body = %q", path, response.Code, response.Body.String())
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
