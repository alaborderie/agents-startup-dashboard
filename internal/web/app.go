package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alaborderie/agents-startup-dashboard/internal/store"
)

const maxRequestBody = 1 << 20

//go:embed templates/*.html static/app.css static/app.js
var webFiles embed.FS

type DataStore interface {
	ApplySnapshot(context.Context, store.Snapshot, []store.Venture, []string, []string) (store.ApplyResult, error)
	ListVentures(context.Context) ([]store.DashboardVenture, error)
	DashboardSpend(context.Context) (store.DashboardSpend, error)
	LastSync(context.Context) (*time.Time, error)
	ApplyLive(context.Context, store.LivePayload) error
	Live(context.Context) (store.LiveState, error)
}

type App struct {
	store          DataStore
	ingestToken    string
	template       *template.Template
	stylesheet     []byte
	stylesheetPath string
	script         []byte
	scriptPath     string
	now            func() time.Time
}

func contentHashPath(name, extension string, body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("/static/%s.%x.%s", name, sum[:6], extension)
}

func New(dataStore DataStore, ingestToken string) (*App, error) {
	stylesheet, err := webFiles.ReadFile("static/app.css")
	if err != nil {
		return nil, err
	}
	script, err := webFiles.ReadFile("static/app.js")
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("index.html").Funcs(template.FuncMap{
		"paragraphs":  paragraphs,
		"tokenFigure": renderTokenFigure,
	}).ParseFS(webFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{
		store:          dataStore,
		ingestToken:    ingestToken,
		template:       tmpl,
		stylesheet:     stylesheet,
		stylesheetPath: contentHashPath("app", "css", stylesheet),
		script:         script,
		scriptPath:     contentHashPath("app", "js", script),
		now:            time.Now,
	}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /robots.txt", robots)
	mux.HandleFunc("GET /live.json", a.liveJSON)
	mux.Handle("POST /ingest", http.TimeoutHandler(http.HandlerFunc(a.ingest), 20*time.Second, ""))
	mux.Handle("POST /ingest/live", http.TimeoutHandler(http.HandlerFunc(a.ingestLive), 20*time.Second, ""))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			a.serveStatic(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case a.stylesheetPath:
		a.writeAsset(w, "text/css; charset=utf-8", a.stylesheet)
	case a.scriptPath:
		a.writeAsset(w, "text/javascript; charset=utf-8", a.script)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) writeAsset(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func robots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	_, _ = io.WriteString(w, "User-agent: *\nDisallow: /\n")
}

type pageData struct {
	Ventures       []ventureView
	Spend          companySpendView
	Agents         []agentSpendView
	LastSync       string
	StylesheetPath string
	ScriptPath     string
}

type tokenFigureView struct {
	Value       string
	Label       string
	Approximate bool
}

type ventureView struct {
	Slug             string
	Title            string
	Field            string
	Status           string
	Stage            string
	Kind             string
	Description      string
	Monetization     string
	AbandonedReason  string
	AbandonedAtStage string
	URL              string
	RepoURL          string
	DesignURL        string
	Spend            *ventureSpendView
	Flow             []flowStepView
}

// flowStepView is one node in the deterministic agent pipeline. State is one
// of "done", "now" or "next" relative to the venture's current stage.
type flowStepView struct {
	Stage string
	Role  string
	Label string
	State string
}

type pipelineStep struct {
	stage string
	role  string
	label string
}

// pipeline is the canonical ordered path a venture walks. Gates and wait
// states are first-class nodes so the picture stays honest about what is
// actually happening, including human approvals.
var pipeline = []pipelineStep{
	{"IDEATION", "inventor", "Ideation"},
	{"SCREENED", "screener", "Screening"},
	{"BUSINESS_CASE", "analyst", "Business case"},
	{"GATE_BUILD", "", "Build gate"},
	{"ARCHITECTURE", "architect", "Architecture"},
	{"BACKLOG", "pm", "Backlog"},
	{"DESIGN", "designer", "Design"},
	{"DESIGN_REVIEW", "", "Design review"},
	{"BUILD", "developer", "Build"},
	{"REVIEW", "reviewer", "Review"},
	{"WAIT_MERGE", "", "Merge"},
	{"INFRA", "devops", "Infra"},
	{"GATE_DEPLOY", "", "Deploy gate"},
	{"WAIT_DEPLOY", "", "Deploy"},
	{"DEPLOYED", "sales", "GTM"},
	{"GTM", "documentation", "Docs"},
}

// pipelineIndex maps a stage to its position in pipeline.
var pipelineIndex = func() map[string]int {
	index := make(map[string]int, len(pipeline))
	for i, step := range pipeline {
		index[step.stage] = i
	}
	return index
}()

func ventureFlow(status, stage string) []flowStepView {
	if status == "abandoned" {
		return nil
	}
	current, known := pipelineIndex[stage]
	flow := make([]flowStepView, 0, len(pipeline))
	for i, step := range pipeline {
		state := "next"
		switch {
		case !known:
			state = "next"
		case i < current:
			state = "done"
		case i == current:
			state = "now"
		default:
			state = "next"
		}
		flow = append(flow, flowStepView{
			Stage: step.stage, Role: step.role, Label: step.label, State: state,
		})
	}
	return flow
}

type ventureSpendView struct {
	Fresh    tokenFigureView
	Total    tokenFigureView
	Runs     string
	Money    string
	BarWidth template.CSS
}

type companySpendView struct {
	HasData       bool
	Meta          string
	Fresh         tokenFigureView
	Total         tokenFigureView
	Orchestration string
	Money         string
	MoneyScope    string
	Unmatched     []unmatchedSpendView
}

type unmatchedSpendView struct {
	Slug  string
	Fresh tokenFigureView
	Runs  string
}

type agentSpendView struct {
	Role     string
	Fresh    tokenFigureView
	Runs     string
	BarWidth template.CSS
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	ventures, err := a.store.ListVentures(r.Context())
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	spend, err := a.store.DashboardSpend(r.Context())
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	lastSync, err := a.store.LastSync(r.Context())
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := buildPageData(ventures, spend)
	view.LastSync = syncLabel(a.now(), lastSync)
	view.StylesheetPath = a.stylesheetPath
	view.ScriptPath = a.scriptPath
	if err := a.template.ExecuteTemplate(w, "index.html", view); err != nil {
		return
	}
}

func buildPageData(ventures []store.DashboardVenture, spend store.DashboardSpend) pageData {
	view := pageData{Ventures: make([]ventureView, 0, len(ventures))}
	var maxVentureFresh int64
	for _, venture := range ventures {
		if venture.Tokens != nil && venture.Tokens.Fresh > maxVentureFresh {
			maxVentureFresh = venture.Tokens.Fresh
		}
	}
	for _, venture := range ventures {
		item := ventureView{
			Slug:  venture.Slug,
			Title: venture.Title, Field: venture.Field, Status: venture.Status,
			Stage: venture.Stage, Kind: venture.Kind, Description: venture.Description,
			Monetization: venture.Monetization, AbandonedReason: venture.AbandonedReason,
			AbandonedAtStage: venture.AbandonedAtStage, URL: venture.URL,
			RepoURL: venture.RepoURL, DesignURL: venture.DesignURL,
			Flow: ventureFlow(venture.Status, venture.Stage),
		}
		if venture.Tokens != nil {
			item.Spend = &ventureSpendView{
				Fresh:    tokenFigure(venture.Tokens.Fresh, "fresh", venture.Tokens.BackfilledRuns > 0),
				Total:    tokenFigure(venture.Tokens.Total, "including cache reads", venture.Tokens.BackfilledRuns > 0),
				Runs:     formatRuns(venture.Tokens.Runs),
				BarWidth: barWidth(venture.Tokens.Fresh, maxVentureFresh),
			}
			if venture.Tokens.CostMeasuredRuns > 0 {
				item.Spend.Money = fmt.Sprintf(
					"$%.2f measured across %d of %d runs — opencode only",
					venture.Tokens.CostUSD, venture.Tokens.CostMeasuredRuns, venture.Tokens.Runs,
				)
			}
		}
		view.Ventures = append(view.Ventures, item)
	}

	if spend.Company == nil {
		return view
	}
	company := spend.Company
	view.Spend.HasData = true
	view.Spend.Meta = spendMeta(spend.FirstTS, spend.LastTS, spend.Records, company.BackfilledRuns)
	view.Spend.Fresh = tokenFigure(company.Fresh, "fresh tokens", company.BackfilledRuns > 0)
	view.Spend.Total = tokenFigure(company.Total, "including cache reads", company.BackfilledRuns > 0)
	view.Spend.Orchestration = orchestrationPercent(company.UnattributedFresh, company.Fresh)
	view.Spend.Money = fmt.Sprintf("$%.2f", company.CostUSD)
	view.Spend.MoneyScope = fmt.Sprintf(
		"measured across %d of %d runs — opencode only; codex and Claude report no price",
		company.CostMeasuredRuns, company.Runs,
	)
	for _, unmatched := range spend.Unmatched {
		view.Spend.Unmatched = append(view.Spend.Unmatched, unmatchedSpendView{
			Slug:  unmatched.Slug,
			Fresh: tokenFigure(unmatched.Fresh, "fresh", unmatched.BackfilledRuns > 0),
			Runs:  formatRuns(unmatched.Runs),
		})
	}

	var maxRoleFresh int64
	for _, role := range spend.Roles {
		if role.Fresh > maxRoleFresh {
			maxRoleFresh = role.Fresh
		}
	}
	for _, role := range spend.Roles {
		view.Agents = append(view.Agents, agentSpendView{
			Role:  role.Role,
			Fresh: tokenFigure(role.Fresh, "fresh", role.BackfilledRuns > 0),
			Runs:  formatRuns(role.Runs), BarWidth: barWidth(role.Fresh, maxRoleFresh),
		})
	}
	return view
}

func tokenFigure(value int64, label string, approximate bool) tokenFigureView {
	return tokenFigureView{Value: formatTokenCount(value), Label: label, Approximate: approximate}
}

func renderTokenFigure(figure tokenFigureView) template.HTML {
	marker := ""
	if figure.Approximate {
		marker = `<span class="approx" aria-label="approximately">≈</span>`
	}
	return template.HTML(`<span class="token-figure"><span class="token-value">` + marker +
		html.EscapeString(figure.Value) + `</span> ` +
		`<span class="token-label">` + html.EscapeString(figure.Label) + `</span></span>`)
}

func formatTokenCount(value int64) string {
	abs := value
	if abs < 0 {
		abs = -abs
	}
	for _, unit := range []struct {
		threshold int64
		suffix    string
	}{{1_000_000_000, "B"}, {1_000_000, "M"}, {1_000, "k"}} {
		if abs >= unit.threshold {
			scaled := float64(value) / float64(unit.threshold)
			precision := 2
			if scaled >= 100 || scaled <= -100 {
				precision = 0
			} else if scaled >= 10 || scaled <= -10 {
				precision = 1
			}
			formatted := strconv.FormatFloat(scaled, 'f', precision, 64)
			if strings.Contains(formatted, ".") {
				formatted = strings.TrimRight(strings.TrimRight(formatted, "0"), ".")
			}
			return formatted + unit.suffix
		}
	}
	return strconv.FormatInt(value, 10)
}

func formatRuns(runs int) string {
	if runs == 1 {
		return "1 run"
	}
	return strconv.Itoa(runs) + " runs"
}

func spendMeta(firstTS, lastTS *time.Time, records, approximate int) string {
	since := "since ledger start"
	if firstTS != nil {
		since = "since " + firstTS.Format("2 Jan 2006")
	}
	meta := fmt.Sprintf("%s · %s", since, formatRuns(records))
	if approximate > 0 {
		meta += fmt.Sprintf(" · ≈ %d approximate", approximate)
	}
	if lastTS != nil {
		meta += " · through " + lastTS.Format("2 Jan 2006 15:04 MST")
	}
	return meta
}

func orchestrationPercent(unattributed, company int64) string {
	if company <= 0 {
		return "—"
	}
	return strconv.FormatFloat(float64(unattributed)*100/float64(company), 'f', 1, 64) + "%"
}

func barWidth(value, maximum int64) template.CSS {
	if maximum <= 0 || value <= 0 {
		return "0%"
	}
	percent := float64(value) * 100 / float64(maximum)
	if percent > 100 {
		percent = 100
	}
	return template.CSS(strconv.FormatFloat(percent, 'f', 1, 64) + "%")
}

func syncLabel(now time.Time, lastSync *time.Time) string {
	if lastSync == nil {
		return "last sync: never"
	}
	minutes := int(now.Sub(*lastSync).Minutes())
	if minutes < 0 {
		minutes = 0
	}
	return "last sync: " + formatMinutes(minutes)
}

func formatMinutes(minutes int) string {
	if minutes == 1 {
		return "1 minute ago"
	}
	return strconv.Itoa(minutes) + " minutes ago"
}

func paragraphs(value string) []string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func (a *App) ingest(w http.ResponseWriter, r *http.Request) {
	if !validBearer(r.Header.Get("Authorization"), a.ingestToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	var snapshot store.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		writeDecodeError(w, err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
		} else {
			writeDecodeError(w, err)
		}
		return
	}

	if len(snapshot.Ventures) == 0 {
		http.Error(w, "ventures must not be empty", http.StatusBadRequest)
		return
	}
	if snapshot.GeneratedAt.IsZero() {
		http.Error(w, "generated_at is required", http.StatusBadRequest)
		return
	}
	if snapshot.GeneratedAt.After(a.now().Add(time.Hour)) {
		http.Error(w, "generated_at is too far in the future", http.StatusBadRequest)
		return
	}

	valid, presentSlugs, warnings := validateVentures(snapshot.Ventures)
	result, err := a.store.ApplySnapshot(r.Context(), snapshot, valid, presentSlugs, warnings)
	if err != nil {
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if result.Status == "unchanged" {
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
		}{Status: result.Status})
		return
	}
	_ = json.NewEncoder(w).Encode(struct {
		Status       string   `json:"status"`
		Applied      int      `json:"applied"`
		SkippedStale int      `json:"skipped_stale"`
		Warnings     []string `json:"warnings"`
		TokenRecords int      `json:"token_records"`
	}{result.Status, result.Applied, result.SkippedStale, result.Warnings, result.TokenRecords})
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "invalid JSON", http.StatusBadRequest)
}

var validFields = map[string]struct{}{
	"development": {}, "testing": {}, "devops": {}, "finance": {},
	"entertainment": {}, "productivity": {}, "health": {}, "education": {},
	"commerce": {}, "marketing": {}, "legal": {}, "logistics": {}, "other": {},
}

var validStatuses = map[string]struct{}{
	"in_progress": {}, "abandoned": {}, "deployed": {},
}

func validateVentures(ventures []store.Venture) ([]store.Venture, []string, []string) {
	valid := make([]store.Venture, 0, len(ventures))
	present := make([]string, 0, len(ventures))
	warnings := make([]string, 0)
	seen := make(map[string]struct{}, len(ventures))

	for _, venture := range ventures {
		if venture.Slug != "" {
			if _, exists := seen[venture.Slug]; !exists {
				present = append(present, venture.Slug)
				seen[venture.Slug] = struct{}{}
			}
		}
		label := venture.Slug
		if label == "" {
			label = "<missing>"
		}
		if venture.Slug == "" || venture.Title == "" {
			warnings = append(warnings, "venture "+label+": slug and title are required")
			continue
		}
		if _, ok := validFields[venture.Field]; !ok {
			warnings = append(warnings, "venture "+label+": unknown field "+venture.Field)
			continue
		}
		if _, ok := validStatuses[venture.Status]; !ok {
			warnings = append(warnings, "venture "+label+": unknown status "+venture.Status)
			continue
		}
		venture.Description = truncate(venture.Description, 600)
		venture.Monetization = truncate(venture.Monetization, 300)
		if venture.DesignURL != nil {
			trimmed := strings.TrimSpace(*venture.DesignURL)
			parsed, err := url.ParseRequestURI(trimmed)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
				warnings = append(warnings, "venture "+label+": invalid design_url")
				venture.DesignURL = nil
			} else {
				venture.DesignURL = &trimmed
			}
		}
		if venture.Stage != nil && *venture.Stage == "DESIGN_REVIEW" && venture.DesignURL == nil {
			warnings = append(warnings, "venture "+label+": DESIGN_REVIEW missing design_url")
		}
		if venture.Status == "abandoned" && (venture.AbandonedReason == nil || strings.TrimSpace(*venture.AbandonedReason) == "") {
			warnings = append(warnings, "venture "+label+": abandoned reason not recorded")
		}
		valid = append(valid, venture)
	}
	return valid, present, warnings
}

func truncate(value *string, limit int) *string {
	if value == nil || utf8.RuneCountInString(*value) <= limit {
		return value
	}
	runes := []rune(*value)
	truncated := string(runes[:limit])
	return &truncated
}

const (
	maxLiveRuns = 64
	maxLogTail  = 64 << 10
)

// ingestLive mirrors the running-agent set. Unlike the venture snapshot this
// is deliberately not idempotent: every push is the complete truth, and an
// empty push means nothing is running.
func (a *App) ingestLive(w http.ResponseWriter, r *http.Request) {
	if !validBearer(r.Header.Get("Authorization"), a.ingestToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	var payload store.LivePayload
	if err := decoder.Decode(&payload); err != nil {
		writeDecodeError(w, err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
		} else {
			writeDecodeError(w, err)
		}
		return
	}

	if payload.GeneratedAt == nil || payload.GeneratedAt.IsZero() {
		http.Error(w, "generated_at is required", http.StatusBadRequest)
		return
	}
	if payload.GeneratedAt.After(a.now().Add(time.Hour)) {
		http.Error(w, "generated_at is too far in the future", http.StatusBadRequest)
		return
	}
	if len(payload.Runs) > maxLiveRuns {
		http.Error(w, "too many live runs", http.StatusBadRequest)
		return
	}

	runs, warnings := validateLiveRuns(payload.Runs)
	payload.Runs = runs
	if err := a.store.ApplyLive(r.Context(), payload); err != nil {
		http.Error(w, "live ingest failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Status   string   `json:"status"`
		Runs     int      `json:"runs"`
		Warnings []string `json:"warnings"`
	}{Status: "ok", Runs: len(runs), Warnings: warnings})
}

func validateLiveRuns(runs []store.LiveRun) ([]store.LiveRun, []string) {
	valid := make([]store.LiveRun, 0, len(runs))
	warnings := make([]string, 0)
	seen := make(map[string]int, len(runs))
	for _, run := range runs {
		if run.RunID == "" || run.Slug == "" || run.Role == "" {
			warnings = append(warnings, "live run skipped: run_id, slug and role are required")
			continue
		}
		if run.Status == "" {
			run.Status = "running"
		}
		if len(run.LogTail) > maxLogTail {
			cut := run.LogTail[len(run.LogTail)-maxLogTail:]
			for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
				cut = cut[1:]
			}
			run.LogTail = "[…]\n" + cut
		}
		if at, exists := seen[run.RunID]; exists {
			valid[at] = run
			continue
		}
		seen[run.RunID] = len(valid)
		valid = append(valid, run)
	}
	return valid, warnings
}

// liveJSON is what the page polls. It is public but never cached, and carries
// a server-computed age so a stalled laptop feed visibly decays.
func (a *App) liveJSON(w http.ResponseWriter, r *http.Request) {
	state, err := a.store.Live(r.Context())
	if err != nil {
		http.Error(w, "live feed unavailable", http.StatusInternalServerError)
		return
	}
	if state.Runs == nil {
		state.Runs = []store.LiveRun{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	_ = json.NewEncoder(w).Encode(state)
}
