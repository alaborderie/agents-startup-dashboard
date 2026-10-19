package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ai-quota caps the response with observations at 1 MiB.
const quotaBodyLimit = 1 << 20
const defaultQuotaURL = "http://ai-quota-viewer.default.svc.cluster.local"

// QuotaView is the dashboard-owned response, separate from the upstream API.
type QuotaView struct {
	State       string          `json:"state"`
	Reason      string          `json:"reason,omitempty"`
	GeneratedAt string          `json:"generated_at,omitempty"`
	Providers   []QuotaProvider `json:"providers,omitempty"`
}

// MarshalJSON keeps an empty provider list distinct from a card without a token.
func (view QuotaView) MarshalJSON() ([]byte, error) {
	if view.State == "ok" {
		return json.Marshal(struct {
			State       string          `json:"state"`
			GeneratedAt string          `json:"generated_at"`
			Providers   []QuotaProvider `json:"providers"`
		}{view.State, view.GeneratedAt, view.Providers})
	}
	return json.Marshal(struct {
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
	}{view.State, view.Reason})
}

type QuotaProvider struct {
	Provider      string        `json:"provider"`
	DisplayName   string        `json:"display_name"`
	Plan          string        `json:"plan,omitempty"`
	Freshness     string        `json:"freshness"`
	StatusState   string        `json:"status_state"`
	StatusMessage string        `json:"status_message,omitempty"`
	Windows       []QuotaWindow `json:"windows"`
	Balance       *QuotaBalance `json:"balance,omitempty"`
	// Readings are native-unit amounts from observations, such as a ledger
	// balance or a billing report; they are never percentages of a quota.
	Readings []QuotaReading `json:"readings,omitempty"`
}

type QuotaReading struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value string `json:"value"`
	// ExpiresAt is when the card must stop showing the reading.
	ExpiresAt string `json:"expires_at"`
}

type QuotaWindow struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	Severity    string  `json:"severity"`
	ResetsAt    string  `json:"resets_at,omitempty"`
	Limited     bool    `json:"limited"`
	// ExpiresAt, for observations only, is when the card must stop showing it.
	ExpiresAt string `json:"expires_at,omitempty"`
}

type QuotaBalance struct {
	AmountUSD float64 `json:"amount_usd"`
	Estimated bool    `json:"estimated"`
	Label     string  `json:"label"`
}

func (a *App) initQuota(secret string) error {
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, "hq-quota-token/v1", 32)
	if err != nil {
		return fmt.Errorf("derive quota encryption key: %w", err)
	}
	a.quotaKey = key
	a.quotaURL = defaultQuotaURL
	a.quotaClient = &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return nil
}

// SetQuotaURL sets the optional, deployment-owned upstream URL at startup.
func (a *App) SetQuotaURL(url string) {
	a.quotaURL = strings.TrimRight(url, "/")
}

func (a *App) quotaAEAD() (cipher.AEAD, error) {
	block, err := aes.NewCipher(a.quotaKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *App) sealQuotaToken(sessionSHA, token string) ([]byte, error) {
	aead, err := a.quotaAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(token), []byte(sessionSHA)), nil
}

func (a *App) openQuotaToken(sessionSHA string, sealed []byte) (string, error) {
	aead, err := a.quotaAEAD()
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("invalid sealed token")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(sessionSHA))
	return string(plain), err
}

func (a *App) quotaSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok, err := a.session(r)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session check failed")
		return "", false
	}
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "login required")
		return "", false
	}
	return id, true
}

// fetchQuota asks for the legacy view with observations. Observations are
// optional, so when ai-quota answers that request with anything but the view
// or a rejected token (it returns 503 when the extension is too large), the
// legacy view alone is requested once instead.
func (a *App) fetchQuota(r *http.Request, token string) (QuotaView, int, error) {
	view, status, err := a.fetchQuotaPath(r, token, "/api/v1/quota?include=observations")
	if err != nil && status != 0 && status != http.StatusUnauthorized && r.Context().Err() == nil {
		return a.fetchQuotaPath(r, token, "/api/v1/quota")
	}
	return view, status, err
}

func (a *App) fetchQuotaPath(r *http.Request, token, path string) (QuotaView, int, error) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.quotaURL+path, nil)
	if err != nil {
		return QuotaView{}, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := a.quotaClient.Do(request)
	if err != nil {
		return QuotaView{}, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return QuotaView{}, response.StatusCode, errors.New("upstream status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, quotaBodyLimit+1))
	if err != nil || len(data) > quotaBodyLimit {
		return QuotaView{}, response.StatusCode, errors.New("upstream body unavailable or oversized")
	}
	view, err := decodeQuota(data, time.Now())
	return view, response.StatusCode, err
}

// decodeQuota pins the subset of ai-quota's v1 contract the card displays:
// the legacy providers, then the products only observations report.
func decodeQuota(data []byte, now time.Time) (QuotaView, error) {
	var upstream struct {
		GeneratedAt string `json:"generated_at"`
		Providers   []struct {
			Provider    string `json:"provider"`
			DisplayName string `json:"display_name"`
			Plan        string `json:"plan"`
			Freshness   string `json:"freshness"`
			Status      struct {
				State   string `json:"state"`
				Message string `json:"message"`
			} `json:"status"`
			Windows []struct {
				ID               string  `json:"id"`
				Label            string  `json:"label"`
				HeadlineEligible bool    `json:"headline_eligible"`
				UsedPercent      float64 `json:"used_percent"`
				Severity         string  `json:"severity"`
				ResetsAt         string  `json:"resets_at"`
				Limited          bool    `json:"limited"`
			} `json:"windows"`
			Balance *QuotaBalance `json:"balance"`
		} `json:"providers"`
		Observations json.RawMessage `json:"observations"`
	}
	if err := json.Unmarshal(data, &upstream); err != nil || upstream.GeneratedAt == "" || upstream.Providers == nil {
		return QuotaView{}, errors.New("invalid upstream quota")
	}
	view := QuotaView{State: "ok", GeneratedAt: upstream.GeneratedAt, Providers: []QuotaProvider{}}
	for _, source := range upstream.Providers {
		switch source.Provider {
		case "claude", "codex", "opencode_go", "opencode_zen":
		default:
			continue
		}
		provider := QuotaProvider{
			Provider: source.Provider, DisplayName: source.DisplayName, Plan: source.Plan,
			Freshness: source.Freshness, StatusState: source.Status.State,
			StatusMessage: source.Status.Message, Windows: []QuotaWindow{}, Balance: source.Balance,
		}
		for _, window := range source.Windows {
			if !window.HeadlineEligible {
				continue
			}
			severity := window.Severity
			switch severity {
			case "ok", "warn", "critical", "limited":
			default:
				severity = "warn"
			}
			provider.Windows = append(provider.Windows, QuotaWindow{
				ID: window.ID, Label: window.Label, UsedPercent: window.UsedPercent,
				Severity: severity, ResetsAt: window.ResetsAt, Limited: window.Limited,
			})
		}
		view.Providers = append(view.Providers, provider)
	}
	view.Providers = append(view.Providers, decodeObservations(upstream.Observations, now)...)
	return view, nil
}

func (a *App) hqQuota(w http.ResponseWriter, r *http.Request) {
	id, ok := a.quotaSession(w, r)
	if !ok {
		return
	}
	sealed, found, err := a.store.QuotaToken(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "quota unavailable")
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, QuotaView{State: "needs_token"})
		return
	}
	token, err := a.openQuotaToken(id, sealed)
	if err != nil {
		if err := a.store.DeleteQuotaToken(r.Context(), id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "quota unavailable")
			return
		}
		writeJSON(w, http.StatusOK, QuotaView{State: "needs_token"})
		return
	}
	view, status, err := a.fetchQuota(r, token)
	if status == http.StatusUnauthorized {
		if err := a.store.DeleteQuotaToken(r.Context(), id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "quota unavailable")
			return
		}
		writeJSON(w, http.StatusOK, QuotaView{State: "needs_token", Reason: "rejected"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, QuotaView{State: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *App) hqQuotaToken(w http.ResponseWriter, r *http.Request) {
	id, ok := a.quotaSession(w, r)
	if !ok {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !decodeOne(w, r, maxSmallBody, &body) {
		return
	}
	if len(body.Token) < 1 || len(body.Token) > 512 || strings.TrimSpace(body.Token) != body.Token {
		writeJSONError(w, http.StatusBadRequest, "token must be 1–512 characters")
		return
	}
	view, status, err := a.fetchQuota(r, body.Token)
	if status == http.StatusUnauthorized {
		writeJSONError(w, http.StatusUnauthorized, "token not accepted")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "ai-quota unreachable; token not saved")
		return
	}
	sealed, err := a.sealQuotaToken(id, body.Token)
	if err != nil || a.store.PutQuotaToken(r.Context(), id, sealed) != nil {
		writeJSONError(w, http.StatusInternalServerError, "quota token not saved")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *App) hqForgetQuotaToken(w http.ResponseWriter, r *http.Request) {
	id, ok := a.quotaSession(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteQuotaToken(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "quota token not forgotten")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
