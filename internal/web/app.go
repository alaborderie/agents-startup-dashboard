package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
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

//go:embed templates/*.html static/app.css
var webFiles embed.FS

type DataStore interface {
	ApplySnapshot(context.Context, store.Snapshot, []store.Venture, []string, []string) (store.ApplyResult, error)
	ListVentures(context.Context) ([]store.DashboardVenture, error)
	LastSync(context.Context) (*time.Time, error)
}

type App struct {
	store          DataStore
	ingestToken    string
	template       *template.Template
	stylesheet     []byte
	stylesheetPath string
	now            func() time.Time
}

func New(dataStore DataStore, ingestToken string) (*App, error) {
	stylesheet, err := webFiles.ReadFile("static/app.css")
	if err != nil {
		return nil, err
	}
	stylesheetHash := sha256.Sum256(stylesheet)
	stylesheetPath := fmt.Sprintf("/static/app.%x.css", stylesheetHash[:6])

	tmpl, err := template.New("index.html").Funcs(template.FuncMap{
		"paragraphs": paragraphs,
	}).ParseFS(webFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{
		store:          dataStore,
		ingestToken:    ingestToken,
		template:       tmpl,
		stylesheet:     stylesheet,
		stylesheetPath: stylesheetPath,
		now:            time.Now,
	}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /robots.txt", robots)
	mux.Handle("POST /ingest", http.TimeoutHandler(http.HandlerFunc(a.ingest), 20*time.Second, ""))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			a.serveStylesheet(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) serveStylesheet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != a.stylesheetPath {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.stylesheet)
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
	Ventures       []store.DashboardVenture
	LastSync       string
	StylesheetPath string
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	ventures, err := a.store.ListVentures(r.Context())
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
	if err := a.template.ExecuteTemplate(w, "index.html", pageData{
		Ventures:       ventures,
		LastSync:       syncLabel(a.now(), lastSync),
		StylesheetPath: a.stylesheetPath,
	}); err != nil {
		return
	}
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
	}{result.Status, result.Applied, result.SkippedStale, result.Warnings})
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
