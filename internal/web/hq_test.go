package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alaborderie/agents-startup-dashboard/internal/store"
	"github.com/coder/websocket"
)

// hqMemory is an in-memory implementation of the HQ half of DataStore.
type hqMemory struct {
	mu          sync.Mutex
	hq          *store.HQState
	links       map[string]*memoryLink
	sessions    map[string]time.Time
	quotaTokens map[string][]byte
	commands    []*store.Command
	clients     map[string]string
	claims      int
}

// postgresText fails like a Postgres text column: it rejects NUL and invalid
// UTF-8, so a handler that forwards either is caught here too.
func postgresText(values ...string) error {
	for _, value := range values {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return errors.New("invalid byte sequence for encoding UTF8")
		}
	}
	return nil
}

type memoryLink struct {
	expires time.Time
	used    bool
}

func (m *hqMemory) ApplyHQ(_ context.Context, generatedAt time.Time, doc json.RawMessage) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hq != nil && generatedAt.Before(m.hq.GeneratedAt) {
		return false, nil
	}
	m.hq = &store.HQState{Doc: doc, GeneratedAt: generatedAt, ReceivedAt: time.Now()}
	return true, nil
}

func (m *hqMemory) HQ(context.Context) (*store.HQState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hq, nil
}

func (m *hqMemory) CreateLoginLink(_ context.Context, hash string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.links == nil {
		m.links = map[string]*memoryLink{}
	}
	m.links[hash] = &memoryLink{expires: expires}
	return nil
}

func (m *hqMemory) ConsumeLoginLink(_ context.Context, hash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	link, ok := m.links[hash]
	if !ok || link.used || !link.expires.After(time.Now()) {
		return false, nil
	}
	link.used = true
	return true, nil
}

func (m *hqMemory) CreateSession(_ context.Context, id string, expires time.Time, userAgent string) error {
	if err := postgresText(userAgent); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]time.Time{}
	}
	m.sessions[id] = expires
	return nil
}

func (m *hqMemory) ValidSession(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	expires, ok := m.sessions[id]
	return ok && expires.After(time.Now()), nil
}

func (m *hqMemory) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	delete(m.quotaTokens, id)
	return nil
}

func (m *hqMemory) PutQuotaToken(_ context.Context, id string, sealed []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quotaTokens == nil {
		m.quotaTokens = map[string][]byte{}
	}
	m.quotaTokens[id] = append([]byte(nil), sealed...)
	return nil
}

func (m *hqMemory) QuotaToken(_ context.Context, id string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sealed, ok := m.quotaTokens[id]
	return append([]byte(nil), sealed...), ok, nil
}

func (m *hqMemory) DeleteQuotaToken(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.quotaTokens, id)
	return nil
}

func (m *hqMemory) CreateCommand(_ context.Context, command store.NewCommand) (store.Command, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clients == nil {
		m.clients = map[string]string{}
	}
	if command.ClientID != "" {
		if id, ok := m.clients[command.ClientID]; ok {
			for _, existing := range m.commands {
				if existing.ID == id {
					return *existing, false, nil
				}
			}
		}
		m.clients[command.ClientID] = command.ID
	}
	created := &store.Command{
		ID: command.ID, Kind: command.Kind, Slug: command.Slug, Gate: command.Gate,
		Text: command.Text, Status: "queued", CreatedAt: time.Now(),
	}
	m.commands = append(m.commands, created)
	return *created, true, nil
}

func (m *hqMemory) ClaimCommands(_ context.Context, redeliver time.Duration, limit int) ([]store.Command, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims++
	claimed := []store.Command{}
	now := time.Now()
	for _, command := range m.commands {
		due := command.Status == "queued" ||
			(command.Status == "delivered" && command.DeliveredAt.Before(now.Add(-redeliver)))
		if due && len(claimed) < limit {
			command.Status = "delivered"
			command.DeliveredAt = &now
			command.Attempts++
			claimed = append(claimed, *command)
		}
	}
	return claimed, nil
}

func (m *hqMemory) FinishCommand(_ context.Context, id, status, message string) (store.Command, bool, bool, error) {
	if err := postgresText(message); err != nil {
		return store.Command{}, false, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, command := range m.commands {
		if command.ID != id {
			continue
		}
		if command.Status == "done" || command.Status == "failed" {
			return *command, true, false, nil
		}
		now := time.Now()
		command.Status, command.Message, command.FinishedAt = status, message, &now
		return *command, true, true, nil
	}
	return store.Command{}, false, false, nil
}

func (m *hqMemory) RecentCommands(_ context.Context, limit int) ([]store.Command, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := []store.Command{}
	for i := len(m.commands) - 1; i >= 0 && len(result) < limit; i-- {
		result = append(result, *m.commands[i])
	}
	return result, nil
}

const testToken = "test-token"

func newHQApp(t *testing.T) (*App, *stubStore) {
	t.Helper()
	stub := &stubStore{}
	app, err := New(stub, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return app, stub
}

func serve(app *App, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	return response
}

func ingestRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	return request
}

// login mints a magic link through the laptop endpoint and exchanges it for a
// session cookie, exactly like the real flow.
func login(t *testing.T, app *App) *http.Cookie {
	t.Helper()
	token := strings.Repeat("a", 43)
	expires := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	response := serve(app, ingestRequest(http.MethodPost, "/ingest/login-links",
		`{"token_sha256":"`+hashHex(token)+`","expires_at":"`+expires+`"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("login link %d: %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/hq/login", strings.NewReader(`{"token":"`+token+`"}`))
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	response = serve(app, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("login %d: %s", response.Code, response.Body.String())
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

func authed(method, path, body string, cookie *http.Cookie) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	return request
}

func TestHQPageWithoutSessionRendersLoginScreenWithStrictHeaders(t *testing.T) {
	app, _ := newHQApp(t)
	response := serve(app, httptest.NewRequest(http.MethodGet, "/hq", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `data-page="login"`) || !strings.Contains(body, "/web") {
		t.Fatalf("login screen missing: %s", body)
	}
	csp := response.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "wss://example.com"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q missing %q", csp, want)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("HQ page must be private: %v", response.Header())
	}
	if !strings.Contains(body, app.assetPath("hq.js")) || !strings.Contains(body, app.assetPath("hq.css")) {
		t.Fatal("HQ page does not reference its content-hashed assets")
	}
}

func TestHQAssetsAreServedAtHashedPaths(t *testing.T) {
	app, _ := newHQApp(t)
	for _, name := range []string{"hq.css", "hq.js"} {
		response := serve(app, httptest.NewRequest(http.MethodGet, app.assetPath(name), nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("%s: status %d", name, response.Code)
		}
	}
}

func TestMagicLinkLoginIsSingleUse(t *testing.T) {
	app, _ := newHQApp(t)
	cookie := login(t, app)
	// __Host- requires Secure, Path=/ and no Domain; a browser drops the
	// cookie otherwise, which would make login silently fail.
	if !strings.HasPrefix(cookie.Name, "__Host-") || !cookie.HttpOnly || !cookie.Secure ||
		cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie is not locked down: %+v", cookie)
	}

	response := serve(app, authed(http.MethodGet, "/hq/api/state", "", cookie))
	if response.Code != http.StatusOK {
		t.Fatalf("state with session: %d %s", response.Code, response.Body.String())
	}
	page := serve(app, authed(http.MethodGet, "/hq", "", cookie)).Body.String()
	if !strings.Contains(page, `data-page="hq"`) {
		t.Fatalf("authenticated page not rendered: %s", page)
	}

	reuse := httptest.NewRequest(http.MethodPost, "/hq/login", strings.NewReader(`{"token":"`+strings.Repeat("a", 43)+`"}`))
	if response := serve(app, reuse); response.Code != http.StatusUnauthorized {
		t.Fatalf("reused link accepted: %d", response.Code)
	}
}

func TestLoginRejectsUnknownAndExpiredLinks(t *testing.T) {
	app, stub := newHQApp(t)
	token := strings.Repeat("b", 43)
	_ = stub.CreateLoginLink(context.Background(), hashHex(token), time.Now().Add(-time.Second))
	for _, candidate := range []string{token, strings.Repeat("c", 43), "short"} {
		request := httptest.NewRequest(http.MethodPost, "/hq/login", strings.NewReader(`{"token":"`+candidate+`"}`))
		if response := serve(app, request); response.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d", candidate, response.Code)
		}
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	app, _ := newHQApp(t)
	cookie := login(t, app)
	response := serve(app, authed(http.MethodPost, "/hq/logout", "", cookie))
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout %d", response.Code)
	}
	cleared := response.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != sessionCookie || cleared[0].Path != "/" || cleared[0].MaxAge >= 0 {
		t.Fatalf("logout did not clear the session cookie: %+v", cleared)
	}
	if response := serve(app, authed(http.MethodGet, "/hq/api/state", "", cookie)); response.Code != http.StatusUnauthorized {
		t.Fatalf("state after logout %d", response.Code)
	}
}

func TestLoginLinkEndpointValidatesAndClampsExpiry(t *testing.T) {
	app, stub := newHQApp(t)
	fixed := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return fixed }
	hash := hashHex("x")
	cases := []struct {
		auth bool
		body string
		want int
	}{
		{false, `{}`, http.StatusUnauthorized},
		{true, `{"token_sha256":"ABC","expires_at":"2026-09-27T12:05:00Z"}`, http.StatusBadRequest},
		{true, `{"token_sha256":"` + hash + `","expires_at":"2026-09-27T11:59:00Z"}`, http.StatusBadRequest},
		{true, `{"token_sha256":"` + hash + `"}`, http.StatusBadRequest},
		{true, `{"token_sha256":"` + hash + `","expires_at":"2026-09-28T12:00:00Z"}`, http.StatusOK},
	}
	for _, c := range cases {
		request := httptest.NewRequest(http.MethodPost, "/ingest/login-links", strings.NewReader(c.body))
		if c.auth {
			request.Header.Set("Authorization", "Bearer "+testToken)
		}
		if response := serve(app, request); response.Code != c.want {
			t.Fatalf("%s: status %d want %d (%s)", c.body, response.Code, c.want, response.Body.String())
		}
	}
	if got := stub.links[hash].expires; !got.Equal(fixed.Add(loginLinkMaxTTL)) {
		t.Fatalf("expiry not clamped: %v", got)
	}
}

func TestHQAPIRequiresSession(t *testing.T) {
	app, _ := newHQApp(t)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/hq/api/state", nil),
		httptest.NewRequest(http.MethodPost, "/hq/api/commands", strings.NewReader(`{"kind":"tick"}`)),
		httptest.NewRequest(http.MethodGet, "/hq/ws", nil),
	} {
		if response := serve(app, request); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status %d", request.Method, request.URL.Path, response.Code)
		}
	}
}

func TestCrossOriginCommandIsRejected(t *testing.T) {
	app, stub := newHQApp(t)
	cookie := login(t, app)
	request := authed(http.MethodPost, "/hq/api/commands", `{"kind":"tick"}`, cookie)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	if response := serve(app, request); response.Code != http.StatusForbidden {
		t.Fatalf("cross-site command status %d", response.Code)
	}
	if len(stub.commands) != 0 {
		t.Fatal("cross-site command was queued")
	}
}

func TestCommandValidation(t *testing.T) {
	app, _ := newHQApp(t)
	cookie := login(t, app)
	long := strings.Repeat("é", 3001)
	cases := []struct {
		body string
		want int
	}{
		{`{"kind":"delete-everything"}`, http.StatusBadRequest},
		{`{"kind":"approve","slug":"../etc","gate":"design"}`, http.StatusBadRequest},
		{`{"kind":"approve","slug":"demo","gate":"merge"}`, http.StatusBadRequest},
		{`{"kind":"changes","slug":"demo","gate":"design","text":"   "}`, http.StatusBadRequest},
		{`{"kind":"note","slug":"demo","text":"` + long + `"}`, http.StatusBadRequest},
		{`{"kind":"suggest","text":"x","client_id":"has space"}`, http.StatusBadRequest},
		{`{"kind":"note","slug":"demo","text":"hi"}{}`, http.StatusBadRequest},
		{`{"kind":"approve","slug":"demo","gate":"design","text":"ignored"}`, http.StatusAccepted},
		{`{"kind":"note","slug":"demo","text":"  keep it short  "}`, http.StatusAccepted},
		{`{"kind":"suggest","text":"tools for game masters"}`, http.StatusAccepted},
		{`{"kind":"tick"}`, http.StatusAccepted},
	}
	for _, c := range cases {
		response := serve(app, authed(http.MethodPost, "/hq/api/commands", c.body, cookie))
		if response.Code != c.want {
			t.Fatalf("%s: status %d want %d (%s)", c.body, response.Code, c.want, response.Body.String())
		}
	}
	var decoded struct{ Commands []store.Command }
	_ = json.Unmarshal(serve(app, authed(http.MethodGet, "/hq/api/state", "", cookie)).Body.Bytes(), &decoded)
	if len(decoded.Commands) != 4 {
		t.Fatalf("queued %d commands, want 4", len(decoded.Commands))
	}
	for _, command := range decoded.Commands {
		if command.Kind == "approve" && command.Text != "" {
			t.Fatal("approve kept text it does not carry")
		}
		if command.Kind == "note" && command.Text != "keep it short" {
			t.Fatalf("note text not trimmed: %q", command.Text)
		}
	}
}

func TestRepeatedClientIDReturnsTheSameCommand(t *testing.T) {
	app, stub := newHQApp(t)
	cookie := login(t, app)
	body := `{"kind":"approve","slug":"demo","gate":"design","client_id":"click-1"}`
	var first, second struct{ Command store.Command }
	_ = json.Unmarshal(serve(app, authed(http.MethodPost, "/hq/api/commands", body, cookie)).Body.Bytes(), &first)
	_ = json.Unmarshal(serve(app, authed(http.MethodPost, "/hq/api/commands", body, cookie)).Body.Bytes(), &second)
	if first.Command.ID == "" || first.Command.ID != second.Command.ID || len(stub.commands) != 1 {
		t.Fatalf("double click queued twice: %q %q (%d)", first.Command.ID, second.Command.ID, len(stub.commands))
	}
}

func TestBridgeLongPollDeliversQueuedCommandsAndRecordsResults(t *testing.T) {
	app, _ := newHQApp(t)
	cookie := login(t, app)

	var empty struct{ Commands []store.Command }
	response := serve(app, ingestRequest(http.MethodGet, "/ingest/commands?wait=0", ""))
	if err := json.Unmarshal(response.Body.Bytes(), &empty); err != nil || empty.Commands == nil || len(empty.Commands) != 0 {
		t.Fatalf("empty poll must return an empty list: %d %s", response.Code, response.Body.String())
	}
	if !app.hub.bridge().Online {
		t.Fatal("a poll must mark the bridge online")
	}

	serve(app, authed(http.MethodPost, "/hq/api/commands", `{"kind":"resume","slug":"demo"}`, cookie))
	var polled struct{ Commands []store.Command }
	_ = json.Unmarshal(serve(app, ingestRequest(http.MethodGet, "/ingest/commands?wait=0", "")).Body.Bytes(), &polled)
	if len(polled.Commands) != 1 || polled.Commands[0].Status != "delivered" || polled.Commands[0].Kind != "resume" {
		t.Fatalf("poll = %+v", polled.Commands)
	}
	id := polled.Commands[0].ID

	if response := serve(app, ingestRequest(http.MethodPost, "/ingest/commands/"+id+"/result", `{"status":"maybe"}`)); response.Code != http.StatusBadRequest {
		t.Fatalf("bad status accepted: %d", response.Code)
	}
	if response := serve(app, ingestRequest(http.MethodPost, "/ingest/commands/c_unknown/result", `{"status":"done"}`)); response.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", response.Code)
	}
	unauthorized := httptest.NewRequest(http.MethodPost, "/ingest/commands/"+id+"/result", strings.NewReader(`{"status":"done"}`))
	if response := serve(app, unauthorized); response.Code != http.StatusUnauthorized {
		t.Fatalf("result without token: %d", response.Code)
	}
	var finished struct{ Command store.Command }
	_ = json.Unmarshal(serve(app, ingestRequest(http.MethodPost, "/ingest/commands/"+id+"/result",
		`{"status":"done","message":"demo resumed. The next tick will retry it."}`)).Body.Bytes(), &finished)
	if finished.Command.Status != "done" || !strings.Contains(finished.Command.Message, "resumed") {
		t.Fatalf("result = %+v", finished.Command)
	}
	var again struct{ Command store.Command }
	_ = json.Unmarshal(serve(app, ingestRequest(http.MethodPost, "/ingest/commands/"+id+"/result",
		`{"status":"failed","message":"late"}`)).Body.Bytes(), &again)
	if again.Command.Status != "done" {
		t.Fatal("a finished command must be final")
	}
}

func TestLongPollWakesWhenACommandIsQueued(t *testing.T) {
	// An absurd wait must clamp to the 30 s maximum, not overflow into an
	// instant return that would make the bridge spin.
	for _, wait := range []string{"10", "9223372037"} {
		app, stub := newHQApp(t)
		cookie := login(t, app)
		done := make(chan []store.Command, 1)
		go func() {
			var polled struct{ Commands []store.Command }
			_ = json.Unmarshal(serve(app, ingestRequest(http.MethodGet, "/ingest/commands?wait="+wait, "")).Body.Bytes(), &polled)
			done <- polled.Commands
		}()
		// Queue only once the poll found the queue empty, so the command can
		// reach it through the wake-up alone (the recheck is 15 s away).
		waitForClaims(t, stub, 1)
		select {
		case commands := <-done:
			t.Fatalf("wait=%s: poll returned %+v before anything was queued", wait, commands)
		case <-time.After(50 * time.Millisecond):
		}
		serve(app, authed(http.MethodPost, "/hq/api/commands", `{"kind":"tick"}`, cookie))
		select {
		case commands := <-done:
			if len(commands) != 1 || commands[0].Kind != "tick" {
				t.Fatalf("wait=%s: woken poll returned %+v", wait, commands)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("wait=%s: long poll did not wake on a new command", wait)
		}
	}
}

func TestBridgeGoesOfflineAfterItStopsPolling(t *testing.T) {
	app, _ := newHQApp(t)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	serve(app, ingestRequest(http.MethodGet, "/ingest/commands?wait=0", ""))
	if _, changed := app.hub.checkBridge(); changed {
		t.Fatal("bridge went offline immediately")
	}
	now = now.Add(bridgeOfflineAfter + time.Second)
	status, changed := app.hub.checkBridge()
	if !changed || status.Online || status.LastSeen == nil {
		t.Fatalf("bridge status = %+v changed=%v", status, changed)
	}
}

func TestIngestHQValidatesStoresAndSkipsStaleDocuments(t *testing.T) {
	app, stub := newHQApp(t)
	fixed := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return fixed }
	cases := []struct {
		body, want string
		code       int
	}{
		{`[1,2]`, "", http.StatusBadRequest},
		{`{"cycle":1}`, "", http.StatusBadRequest},
		{`{"generated_at":"2026-09-27T14:00:00Z"}`, "", http.StatusBadRequest},
		{`{"generated_at":"2026-09-27T12:00:00Z", "cycle": 229}`, "ok", http.StatusOK},
		{`{"generated_at":"2026-09-27T11:00:00Z","cycle":228}`, "stale", http.StatusOK},
	}
	for _, c := range cases {
		response := serve(app, ingestRequest(http.MethodPost, "/ingest/hq", c.body))
		if response.Code != c.code || (c.want != "" && !strings.Contains(response.Body.String(), c.want)) {
			t.Fatalf("%s: %d %s", c.body, response.Code, response.Body.String())
		}
	}
	if string(stub.hq.Doc) != `{"generated_at":"2026-09-27T12:00:00Z","cycle":229}` {
		t.Fatalf("stored doc = %s", stub.hq.Doc)
	}
	if response := serve(app, httptest.NewRequest(http.MethodPost, "/ingest/hq", strings.NewReader(`{}`))); response.Code != http.StatusUnauthorized {
		t.Fatalf("hq ingest without token: %d", response.Code)
	}
}

func TestStateCombinesPublicLiveAndPrivateViews(t *testing.T) {
	app, stub := newHQApp(t)
	cookie := login(t, app)
	stub.ventures = []store.DashboardVenture{{Slug: "demo", Title: "Demo", Field: "development", Status: "in_progress", Stage: "BUILD"}}
	started := time.Now()
	stub.live = store.LiveState{TickRunning: true, Runs: []store.LiveRun{{RunID: "r", Slug: "demo", Role: "developer", StartedAt: &started}}}
	serve(app, ingestRequest(http.MethodPost, "/ingest/hq", `{"generated_at":"2026-09-27T12:00:00Z","ventures":[{"slug":"demo","gate":"design"}]}`))

	var state map[string]json.RawMessage
	if err := json.Unmarshal(serve(app, authed(http.MethodGet, "/hq/api/state", "", cookie)).Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(state))
	for key := range state {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "bridge,commands,hq,live,server_time,status,type,ventures" {
		t.Fatalf("state keys = %v", keys)
	}
	if !strings.Contains(string(state["ventures"]), `"stage":"BUILD"`) || !strings.Contains(string(state["ventures"]), `"repo_url":null`) {
		t.Fatalf("ventures = %s", state["ventures"])
	}
	if !strings.Contains(string(state["hq"]), `"gate":"design"`) || !strings.Contains(string(state["live"]), `"role":"developer"`) {
		t.Fatalf("hq/live missing: %s %s", state["hq"], state["live"])
	}
}

func TestWebSocketStreamsStateThenDeltas(t *testing.T) {
	app, _ := newHQApp(t)
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	cookie := login(t, app)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A cross-origin handshake must fail even with a valid cookie.
	crossHeaders := http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}, "Origin": {"https://evil.example"}}
	if conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/hq/ws", &websocket.DialOptions{HTTPHeader: crossHeaders}); err == nil {
		conn.CloseNow()
		t.Fatal("cross-origin WebSocket accepted")
	}
	if conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/hq/ws", nil); err == nil {
		conn.CloseNow()
		t.Fatal("WebSocket without a session accepted")
	}

	headers := http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}, "Origin": {server.URL}}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/hq/ws", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	read := func() map[string]json.RawMessage {
		t.Helper()
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		return message
	}
	if kind := string(read()["type"]); kind != `"state"` {
		t.Fatalf("first message type %s", kind)
	}

	live := `{"generated_at":"2026-09-27T12:00:00Z","cycle":7,"tick_running":true,"runs":[{"run_id":"r1","slug":"demo","role":"reviewer","log_tail":"reading"}]}`
	serve(app, ingestRequest(http.MethodPost, "/ingest/live", live))
	message := read()
	if string(message["type"]) != `"live"` || !strings.Contains(string(message["live"]), `"role":"reviewer"`) {
		t.Fatalf("live delta = %v", message)
	}

	serve(app, authed(http.MethodPost, "/hq/api/commands", `{"kind":"note","slug":"demo","text":"ship it"}`, cookie))
	message = read()
	if string(message["type"]) != `"command"` || !strings.Contains(string(message["command"]), `"status":"queued"`) {
		t.Fatalf("command delta = %v", message)
	}

	serve(app, ingestRequest(http.MethodPost, "/ingest/hq", `{"generated_at":"2026-09-27T12:00:00Z","cycle":7}`))
	message = read()
	if string(message["type"]) != `"hq"` || !strings.Contains(string(message["hq"]), `"cycle":7`) {
		t.Fatalf("hq delta = %v", message)
	}
}

func TestSlowSubscriberIsDroppedInsteadOfBlockingBroadcasts(t *testing.T) {
	h := newHub(time.Now)
	slow := h.subscribe()
	for i := 0; i < subscriberBuffer+1; i++ {
		h.broadcast([]byte("x"))
	}
	select {
	case <-slow.dropped:
	default:
		t.Fatal("a subscriber that stopped reading was kept")
	}
	if h.hasSubscribers() {
		t.Fatal("dropped subscriber still registered")
	}
}

func TestLoginSurvivesAUserAgentThatIsNotUTF8(t *testing.T) {
	app, stub := newHQApp(t)
	token := strings.Repeat("d", 43)
	_ = stub.CreateLoginLink(context.Background(), hashHex(token), time.Now().Add(time.Minute))
	request := httptest.NewRequest(http.MethodPost, "/hq/login", strings.NewReader(`{"token":"`+token+`"}`))
	request.Header.Set("User-Agent", "Mozilla/5.0 \xff\xfe")
	if response := serve(app, request); response.Code != http.StatusNoContent {
		t.Fatalf("login with a non-UTF-8 user agent: %d %s", response.Code, response.Body.String())
	}
}

func TestCommandResultDropsNULFromTheMessage(t *testing.T) {
	app, stub := newHQApp(t)
	_, _, _ = stub.CreateCommand(context.Background(), store.NewCommand{ID: "c_nul", Kind: "tick"})
	response := serve(app, ingestRequest(http.MethodPost, "/ingest/commands/c_nul/result",
		`{"status":"failed","message":"tick failed: \u0000garbage"}`))
	var result struct{ Command store.Command }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK {
		t.Fatalf("result with NUL: %d %s", response.Code, response.Body.String())
	}
	if result.Command.Status != "failed" || result.Command.Message != "tick failed: garbage" {
		t.Fatalf("result = %+v", result.Command)
	}
}

// waitForClaims blocks until the bridge's long poll has queried the queue n
// times, so a test knows the poll is parked rather than about to start.
func waitForClaims(t *testing.T, stub *stubStore, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		stub.mu.Lock()
		claims := stub.claims
		stub.mu.Unlock()
		if claims >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("long poll queried the queue %d times, want %d", claims, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Shutdown waits for in-flight requests, and a parked long poll is one: Close,
// registered as main registers it, must release the poll within the budget.
func TestShutdownReleasesTheBridgeLongPoll(t *testing.T) {
	app, stub := newHQApp(t)
	server := httptest.NewUnstartedServer(app.Handler())
	server.Config.RegisterOnShutdown(app.Close)
	server.Start()
	defer server.Close()

	polled := make(chan string, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/ingest/commands?wait=30", nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			polled <- err.Error()
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		polled <- strconv.Itoa(response.StatusCode) + " " + strings.TrimSpace(string(body))
	}()
	waitForClaims(t, stub, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Config.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown waited out the long poll: %v", err)
	}
	if got := <-polled; got != `200 {"commands":[]}` {
		t.Fatalf("released poll = %s", got)
	}
}

func TestCloseEndsSocketsIncludingOnesThatConnectAfterwards(t *testing.T) {
	app, _ := newHQApp(t)
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	cookie := login(t, app)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func() *websocket.Conn {
		t.Helper()
		headers := http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}, "Origin": {server.URL}}
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/hq/ws", &websocket.DialOptions{HTTPHeader: headers})
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	readUntilClosed := func(conn *websocket.Conn) websocket.StatusCode {
		t.Helper()
		defer conn.CloseNow()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return websocket.CloseStatus(err)
			}
		}
	}

	before := dial()
	if _, _, err := before.Read(ctx); err != nil { // the initial state
		t.Fatal(err)
	}
	app.Close()
	if code := readUntilClosed(before); code != websocket.StatusGoingAway {
		t.Fatalf("open socket closed with %v, want going away", code)
	}
	if code := readUntilClosed(dial()); code != websocket.StatusGoingAway {
		t.Fatalf("late socket closed with %v, want going away", code)
	}
}

// A socket outlives its handshake, so logging out (or the session expiring)
// must end it at the next ping rather than keep streaming private state.
func TestWebSocketEndsWhenTheSessionEnds(t *testing.T) {
	app, _ := newHQApp(t)
	app.wsPing = 20 * time.Millisecond
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	cookie := login(t, app)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headers := http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}, "Origin": {server.URL}}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/hq/ws", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil { // the initial state
		t.Fatal(err)
	}
	if response := serve(app, authed(http.MethodPost, "/hq/logout", "", cookie)); response.Code != http.StatusNoContent {
		t.Fatalf("logout %d", response.Code)
	}
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			if code := websocket.CloseStatus(err); code != wsSessionEnded {
				t.Fatalf("socket ended with %v (%v), want %d", code, err, wsSessionEnded)
			}
			return
		}
	}
}

func TestBrowsersSeeACommandQueuedBeforeDelivered(t *testing.T) {
	app, stub := newHQApp(t)
	cookie := login(t, app)
	browser := app.hub.subscribe()
	defer app.hub.unsubscribe(browser)
	done := make(chan struct{})
	go func() {
		serve(app, ingestRequest(http.MethodGet, "/ingest/commands?wait=10", ""))
		close(done)
	}()
	waitForClaims(t, stub, 1)
	serve(app, authed(http.MethodPost, "/hq/api/commands", `{"kind":"tick"}`, cookie))
	<-done
	var statuses []string
	for len(browser.messages) > 0 {
		var message struct{ Command *store.Command }
		if err := json.Unmarshal(<-browser.messages, &message); err == nil && message.Command != nil {
			statuses = append(statuses, message.Command.Status)
		}
	}
	if strings.Join(statuses, ",") != "queued,delivered" {
		t.Fatalf("command deltas = %v, want queued then delivered", statuses)
	}
}
