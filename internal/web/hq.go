package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alaborderie/agents-startup-dashboard/internal/store"
	"github.com/coder/websocket"
)

const (
	sessionCookie    = "hq_session"
	sessionTTL       = 30 * 24 * time.Hour
	loginLinkMaxTTL  = 15 * time.Minute
	maxHQBody        = 2 << 20
	maxSmallBody     = 16 << 10
	redeliverAfter   = 120 * time.Second
	defaultPollWait  = 25 * time.Second
	maxPollWait      = 30 * time.Second
	pollRecheck      = 15 * time.Second
	wsPingInterval   = 25 * time.Second
	wsWriteTimeout   = 10 * time.Second
	recentCommandCap = 50
	// wsSessionEnded tells the page to reload into the login screen instead
	// of reconnecting forever.
	wsSessionEnded websocket.StatusCode = 4001
)

var (
	slugPattern      = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,60}$`)
	clientIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tokenHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostPattern      = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]{1,255}$`)
)

// commandSpec says what each founder action needs. The laptop validates again
// against the on-disk truth; this only keeps garbage out of the queue.
type commandSpec struct {
	slug, gate bool
	maxText    int // 0: the command carries no text
}

var commandSpecs = map[string]commandSpec{
	"approve": {slug: true, gate: true},
	"changes": {slug: true, gate: true, maxText: 3000},
	"reject":  {slug: true, gate: true, maxText: 3000},
	"note":    {slug: true, maxText: 3000},
	"answer":  {slug: true, maxText: 3000},
	"resume":  {slug: true},
	"suggest": {maxText: 2000},
	"tick":    {},
}

var validGates = map[string]struct{}{"build": {}, "design": {}, "deploy": {}}

type hqPageData struct {
	Authenticated  bool
	StylesheetPath string
	ScriptPath     string
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// decodeOne decodes exactly one JSON object from a bounded body.
func decodeOne(w http.ResponseWriter, r *http.Request, limit int64, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		}
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "request must contain one JSON object")
		return false
	}
	return true
}

func (a *App) hqHeaders(w http.ResponseWriter, r *http.Request) {
	connect := "'self'"
	if hostPattern.MatchString(r.Host) {
		connect += " wss://" + r.Host
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
			connect += " ws://" + r.Host
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; "+
		"style-src 'self'; img-src 'self' data:; connect-src "+connect+"; "+
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// session returns the hashed session id when the request carries a live
// session.
func (a *App) session(r *http.Request) (string, bool, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || len(cookie.Value) < 32 || len(cookie.Value) > 128 {
		return "", false, nil
	}
	id := hashHex(cookie.Value)
	ok, err := a.store.ValidSession(r.Context(), id)
	return id, ok, err
}

func (a *App) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ok, err := a.session(r)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "session check failed")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "login required")
			return
		}
		next(w, r)
	}
}

func (a *App) hqPage(w http.ResponseWriter, r *http.Request) {
	_, ok, err := a.session(r)
	if err != nil {
		http.Error(w, "HQ unavailable", http.StatusInternalServerError)
		return
	}
	a.renderHQ(w, r, "hq.html", ok)
}

func (a *App) hqLoginPage(w http.ResponseWriter, r *http.Request) {
	a.renderHQ(w, r, "hq-login.html", false)
}

func (a *App) renderHQ(w http.ResponseWriter, r *http.Request, name string, authenticated bool) {
	a.hqHeaders(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.template.ExecuteTemplate(w, name, hqPageData{
		Authenticated:  authenticated,
		StylesheetPath: a.assetPath("hq.css"),
		ScriptPath:     a.assetPath("hq.js"),
	})
}

// hqLogin exchanges a one-time magic-link token for a session cookie.
func (a *App) hqLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeOne(w, r, maxSmallBody, &body) {
		return
	}
	if len(body.Token) < 32 || len(body.Token) > 128 {
		writeJSONError(w, http.StatusUnauthorized, "link expired or already used")
		return
	}
	ok, err := a.store.ConsumeLoginLink(r.Context(), hashHex(body.Token))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login failed")
		return
	}
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "link expired or already used")
		return
	}
	sessionID, err := randomToken(32)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login failed")
		return
	}
	// Header values may carry any byte but NUL; a text column accepts only
	// UTF-8, and the link is already spent if this insert fails.
	userAgent := strings.ToValidUTF8(r.UserAgent(), "")
	if len(userAgent) > 200 {
		userAgent = strings.ToValidUTF8(userAgent[:200], "")
	}
	if err := a.store.CreateSession(r.Context(), hashHex(sessionID), a.now().Add(sessionTTL), userAgent); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login failed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sessionID, Path: "/hq",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) hqLogout(w http.ResponseWriter, r *http.Request) {
	if id, ok, err := a.session(r); err == nil && ok {
		if err := a.store.DeleteSession(r.Context(), id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "logout failed")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/hq", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// publicVenture is the JSON form of a public dashboard row.
type publicVenture struct {
	Slug             string        `json:"slug"`
	Title            string        `json:"title"`
	Field            string        `json:"field"`
	Status           string        `json:"status"`
	Stage            *string       `json:"stage"`
	Kind             *string       `json:"kind"`
	Description      *string       `json:"description"`
	Monetization     *string       `json:"monetization"`
	AbandonedReason  *string       `json:"abandoned_reason"`
	AbandonedAtStage *string       `json:"abandoned_at_stage"`
	URL              *string       `json:"url"`
	RepoURL          *string       `json:"repo_url"`
	DesignURL        *string       `json:"design_url"`
	Created          *time.Time    `json:"created"`
	Tokens           *publicTokens `json:"tokens"`
}

type publicTokens struct {
	Runs    int     `json:"runs"`
	Fresh   int64   `json:"fresh"`
	Total   int64   `json:"total"`
	CostUSD float64 `json:"cost_usd"`
}

func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func publicVentures(ventures []store.DashboardVenture) []publicVenture {
	result := make([]publicVenture, 0, len(ventures))
	for _, v := range ventures {
		item := publicVenture{
			Slug: v.Slug, Title: v.Title, Field: v.Field, Status: v.Status,
			Stage: nullable(v.Stage), Kind: nullable(v.Kind),
			Description: nullable(v.Description), Monetization: nullable(v.Monetization),
			AbandonedReason: nullable(v.AbandonedReason), AbandonedAtStage: nullable(v.AbandonedAtStage),
			URL: nullable(v.URL), RepoURL: nullable(v.RepoURL), DesignURL: nullable(v.DesignURL),
			Created: v.CreatedAt,
		}
		if v.Tokens != nil {
			item.Tokens = &publicTokens{
				Runs: v.Tokens.Runs, Fresh: v.Tokens.Fresh, Total: v.Tokens.Total, CostUSD: v.Tokens.CostUSD,
			}
		}
		result = append(result, item)
	}
	return result
}

type hqEnvelope struct {
	ReceivedAt time.Time       `json:"received_at"`
	AgeSeconds int             `json:"age_seconds"`
	Doc        json.RawMessage `json:"doc"`
}

type hqStateMessage struct {
	Type       string               `json:"type"`
	ServerTime time.Time            `json:"server_time"`
	Ventures   []publicVenture      `json:"ventures"`
	Live       store.LiveState      `json:"live"`
	Status     store.SnapshotStatus `json:"status"`
	HQ         *hqEnvelope          `json:"hq"`
	Commands   []store.Command      `json:"commands"`
	Bridge     bridgeStatus         `json:"bridge"`
}

func (a *App) hqState(ctx context.Context) (hqStateMessage, error) {
	ventures, err := a.store.ListVentures(ctx)
	if err != nil {
		return hqStateMessage{}, err
	}
	live, err := a.store.Live(ctx)
	if err != nil {
		return hqStateMessage{}, err
	}
	if live.Runs == nil {
		live.Runs = []store.LiveRun{}
	}
	status, err := a.store.SnapshotStatus(ctx)
	if err != nil {
		return hqStateMessage{}, err
	}
	hq, err := a.store.HQ(ctx)
	if err != nil {
		return hqStateMessage{}, err
	}
	commands, err := a.store.RecentCommands(ctx, recentCommandCap)
	if err != nil {
		return hqStateMessage{}, err
	}
	message := hqStateMessage{
		Type: "state", ServerTime: a.now().UTC(), Ventures: publicVentures(ventures),
		Live: live, Status: status, Commands: commands, Bridge: a.hub.bridge(),
	}
	if hq != nil {
		message.HQ = a.envelope(hq.ReceivedAt, hq.Doc)
	}
	return message, nil
}

func (a *App) envelope(received time.Time, doc json.RawMessage) *hqEnvelope {
	age := int(a.now().Sub(received).Seconds())
	if age < 0 {
		age = 0
	}
	return &hqEnvelope{ReceivedAt: received.UTC(), AgeSeconds: age, Doc: doc}
}

func (a *App) hqStateJSON(w http.ResponseWriter, r *http.Request) {
	state, err := a.hqState(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "state unavailable")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// hqCommand queues a founder action for the laptop bridge.
func (a *App) hqCommand(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind     string `json:"kind"`
		Slug     string `json:"slug"`
		Gate     string `json:"gate"`
		Text     string `json:"text"`
		ClientID string `json:"client_id"`
	}
	if !decodeOne(w, r, maxSmallBody, &body) {
		return
	}
	command, problem := validateCommand(body.Kind, body.Slug, body.Gate, body.Text, body.ClientID)
	if problem != "" {
		writeJSONError(w, http.StatusBadRequest, problem)
		return
	}
	id, err := randomToken(12)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "command failed")
		return
	}
	command.ID = "c_" + id
	stored, created, err := a.store.CreateCommand(r.Context(), command)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "command failed")
		return
	}
	if created {
		a.hub.notifyCommand()
		a.broadcastCommand(stored)
	}
	writeJSON(w, http.StatusAccepted, map[string]store.Command{"command": stored})
}

func validateCommand(kind, slug, gate, text, clientID string) (store.NewCommand, string) {
	spec, ok := commandSpecs[kind]
	if !ok {
		return store.NewCommand{}, "unknown command kind"
	}
	command := store.NewCommand{Kind: kind}
	if spec.slug {
		if !slugPattern.MatchString(slug) {
			return store.NewCommand{}, "invalid venture slug"
		}
		command.Slug = slug
	}
	if spec.gate {
		if _, ok := validGates[gate]; !ok {
			return store.NewCommand{}, "gate must be build, design or deploy"
		}
		command.Gate = gate
	}
	if spec.maxText > 0 {
		text = strings.TrimSpace(text)
		if text == "" {
			return store.NewCommand{}, "text is required"
		}
		if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return store.NewCommand{}, "text must be valid UTF-8"
		}
		if utf8.RuneCountInString(text) > spec.maxText {
			return store.NewCommand{}, "text must be at most " + strconv.Itoa(spec.maxText) + " characters"
		}
		command.Text = text
	}
	if clientID != "" {
		if !clientIDPattern.MatchString(clientID) {
			return store.NewCommand{}, "invalid client_id"
		}
		command.ClientID = clientID
	}
	return command, ""
}

func encodeMessage(value any) []byte {
	message, err := json.Marshal(value)
	if err != nil {
		log.Printf("hq: encode message: %v", err)
		return nil
	}
	return message
}

func (a *App) broadcastCommand(command store.Command) {
	a.hub.broadcast(encodeMessage(struct {
		Type    string        `json:"type"`
		Command store.Command `json:"command"`
	}{"command", command}))
}

func (a *App) broadcastBridge(status bridgeStatus) {
	a.hub.broadcast(encodeMessage(struct {
		Type   string       `json:"type"`
		Bridge bridgeStatus `json:"bridge"`
	}{"bridge", status}))
}

func (a *App) broadcastLive(payload store.LivePayload) {
	if !a.hub.hasSubscribers() {
		return
	}
	runs := payload.Runs
	if runs == nil {
		runs = []store.LiveRun{}
	}
	a.hub.broadcast(encodeMessage(struct {
		Type string          `json:"type"`
		Live store.LiveState `json:"live"`
	}{"live", store.LiveState{
		GeneratedAt: payload.GeneratedAt, Cycle: payload.Cycle,
		TickRunning: payload.TickRunning, Runs: runs,
	}}))
}

func (a *App) broadcastVentures(ctx context.Context) {
	if !a.hub.hasSubscribers() {
		return
	}
	ventures, err := a.store.ListVentures(ctx)
	if err != nil {
		log.Printf("hq: list ventures for broadcast: %v", err)
		return
	}
	status, err := a.store.SnapshotStatus(ctx)
	if err != nil {
		log.Printf("hq: snapshot status for broadcast: %v", err)
		return
	}
	a.hub.broadcast(encodeMessage(struct {
		Type     string               `json:"type"`
		Ventures []publicVenture      `json:"ventures"`
		Status   store.SnapshotStatus `json:"status"`
	}{"ventures", publicVentures(ventures), status}))
}

// hqWebSocket streams the full state once, then every change, to one browser.
func (a *App) hqWebSocket(w http.ResponseWriter, r *http.Request) {
	sessionID, ok, err := a.session(r)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session check failed")
		return
	}
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "login required")
		return
	}
	// Accept rejects a cross-origin handshake: Origin must match Host.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	// Subscribe before reading the state so nothing that lands in between is
	// lost; every delta is a replacement, so a duplicate is harmless.
	subscription := a.hub.subscribe()
	defer a.hub.unsubscribe(subscription)
	// The browser never sends data; CloseRead answers pings and notices close.
	ctx := conn.CloseRead(r.Context())

	state, err := a.hqState(ctx)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "state unavailable")
		return
	}
	if !a.writeMessage(ctx, conn, encodeMessage(state)) {
		return
	}

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-subscription.dropped:
			select {
			case <-a.hub.shuttingDown():
				conn.Close(websocket.StatusGoingAway, "server restarting")
			default:
				conn.Close(websocket.StatusTryAgainLater, "reconnect for a fresh state")
			}
			return
		case message := <-subscription.messages:
			if message != nil && !a.writeMessage(ctx, conn, message) {
				return
			}
		case <-ping.C:
			pingContext, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Ping(pingContext)
			cancel()
			if err != nil {
				return
			}
			if valid, err := a.store.ValidSession(ctx, sessionID); err == nil && !valid {
				conn.Close(wsSessionEnded, "session ended")
				return
			}
		}
	}
}

func (a *App) writeMessage(ctx context.Context, conn *websocket.Conn, message []byte) bool {
	writeContext, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return conn.Write(writeContext, websocket.MessageText, message) == nil
}

// ingestHQ stores the laptop's private HQ document and fans it out.
func (a *App) ingestHQ(w http.ResponseWriter, r *http.Request) {
	if !validBearer(r.Header.Get("Authorization"), a.ingestToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHQBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil || !bytes.HasPrefix(compact.Bytes(), []byte("{")) {
		http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
		return
	}
	var header struct {
		GeneratedAt *time.Time `json:"generated_at"`
	}
	if err := json.Unmarshal(compact.Bytes(), &header); err != nil || header.GeneratedAt == nil || header.GeneratedAt.IsZero() {
		http.Error(w, "generated_at is required", http.StatusBadRequest)
		return
	}
	if header.GeneratedAt.After(a.now().Add(time.Hour)) {
		http.Error(w, "generated_at is too far in the future", http.StatusBadRequest)
		return
	}
	doc := json.RawMessage(compact.Bytes())
	applied, err := a.store.ApplyHQ(r.Context(), *header.GeneratedAt, doc)
	if err != nil {
		http.Error(w, "hq ingest failed", http.StatusInternalServerError)
		return
	}
	if !applied {
		writeJSON(w, http.StatusOK, map[string]string{"status": "stale"})
		return
	}
	if a.hub.hasSubscribers() {
		a.hub.broadcast(encodeMessage(struct {
			Type string      `json:"type"`
			HQ   *hqEnvelope `json:"hq"`
		}{"hq", a.envelope(a.now(), doc)}))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ingestLoginLink records the hash of a magic-link token minted on the laptop.
func (a *App) ingestLoginLink(w http.ResponseWriter, r *http.Request) {
	if !validBearer(r.Header.Get("Authorization"), a.ingestToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		TokenSHA256 string     `json:"token_sha256"`
		ExpiresAt   *time.Time `json:"expires_at"`
	}
	if !decodeOne(w, r, maxSmallBody, &body) {
		return
	}
	if !tokenHashPattern.MatchString(body.TokenSHA256) {
		writeJSONError(w, http.StatusBadRequest, "token_sha256 must be 64 lowercase hex characters")
		return
	}
	now := a.now()
	if body.ExpiresAt == nil || !body.ExpiresAt.After(now) {
		writeJSONError(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}
	expires := *body.ExpiresAt
	if limit := now.Add(loginLinkMaxTTL); expires.After(limit) {
		expires = limit
	}
	if err := a.store.CreateLoginLink(r.Context(), body.TokenSHA256, expires); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login link failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "expires_at": expires.UTC().Format(time.RFC3339)})
}

// pollCommands is the laptop bridge's long poll for queued founder actions.
func (a *App) pollCommands(w http.ResponseWriter, r *http.Request) {
	if !validBearer(r.Header.Get("Authorization"), a.ingestToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	wait := defaultPollWait
	if raw := r.URL.Query().Get("wait"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 {
			writeJSONError(w, http.StatusBadRequest, "wait must be a non-negative number of seconds")
			return
		}
		// Clamp before converting: a huge value would overflow to a negative
		// duration and return at once.
		wait = time.Duration(min(seconds, int(maxPollWait/time.Second))) * time.Second
	}
	if a.hub.pollStarted() {
		a.broadcastBridge(a.hub.bridge())
	}
	defer a.hub.pollEnded()

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	recheck := time.NewTicker(pollRecheck)
	defer recheck.Stop()
	for {
		signal := a.hub.commandSignal()
		commands, err := a.store.ClaimCommands(r.Context(), redeliverAfter, 20)
		if err != nil {
			if r.Context().Err() == nil {
				writeJSONError(w, http.StatusInternalServerError, "command poll failed")
			}
			return
		}
		if len(commands) > 0 {
			for _, command := range commands {
				a.broadcastCommand(command)
			}
			writeJSON(w, http.StatusOK, map[string][]store.Command{"commands": commands})
			return
		}
		select {
		case <-signal:
		case <-recheck.C:
		case <-deadline.C:
			writeJSON(w, http.StatusOK, map[string][]store.Command{"commands": {}})
			return
		case <-a.hub.shuttingDown():
			// Answer rather than hold Shutdown up; the bridge polls again.
			writeJSON(w, http.StatusOK, map[string][]store.Command{"commands": {}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// commandResult records what the laptop did with a command.
func (a *App) commandResult(w http.ResponseWriter, r *http.Request) {
	if !validBearer(r.Header.Get("Authorization"), a.ingestToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if !decodeOne(w, r, maxSmallBody, &body) {
		return
	}
	if body.Status != "done" && body.Status != "failed" {
		writeJSONError(w, http.StatusBadRequest, "status must be done or failed")
		return
	}
	// A text column rejects NUL; the bridge may relay raw process output, and
	// a result the server cannot store would leave the command unfinished.
	message := strings.ReplaceAll(strings.ToValidUTF8(body.Message, ""), "\x00", "")
	if utf8.RuneCountInString(message) > 2000 {
		message = string([]rune(message)[:2000])
	}
	command, found, changed, err := a.store.FinishCommand(r.Context(), r.PathValue("id"), body.Status, message)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "command result failed")
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "unknown command")
		return
	}
	if changed {
		a.broadcastCommand(command)
	}
	writeJSON(w, http.StatusOK, map[string]store.Command{"command": command})
}

// Run watches the bridge's presence until ctx ends.
func (a *App) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if status, changed := a.hub.checkBridge(); changed {
				a.broadcastBridge(status)
			}
		}
	}
}

// Close ends every HQ socket and answers every parked long poll. Register it
// with http.Server.RegisterOnShutdown: Shutdown never sees hijacked sockets
// and would otherwise wait out a bridge poll.
func (a *App) Close() {
	a.hub.closeAll()
}
