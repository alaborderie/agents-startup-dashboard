package web

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const quotaReply = `{"generated_at":"2026-09-29T09:00:00Z","providers":[]}`
const quotaReadToken = "synthetic-quota-test-token"

func TestQuotaTokenFlowIsSessionBoundAndForgetsOnLogout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quota" || r.URL.Query().Get("include") != "observations" || r.Header.Get("Authorization") != "Bearer "+quotaReadToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, quotaReply)
	}))
	defer upstream.Close()
	app, stub := newHQApp(t)
	app.quotaURL = upstream.URL
	first := login(t, app)
	second := login(t, app)
	get := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		return serve(app, authed(http.MethodGet, "/hq/api/quota", "", cookie))
	}
	if got := get(first); got.Code != 200 || !strings.Contains(got.Body.String(), `"needs_token"`) {
		t.Fatalf("initial quota: %d %s", got.Code, got.Body.String())
	}
	put := serve(app, authed(http.MethodPut, "/hq/api/quota/token", `{"token":"`+quotaReadToken+`"}`, first))
	if put.Code != 200 || !strings.Contains(put.Body.String(), `"state":"ok"`) {
		t.Fatalf("verified token: %d %s", put.Code, put.Body.String())
	}
	if got := get(first); got.Code != 200 || !strings.Contains(got.Body.String(), `"state":"ok"`) {
		t.Fatalf("stored quota: %d %s", got.Code, got.Body.String())
	}
	if got := get(first); !strings.Contains(got.Body.String(), `"providers":[]`) {
		t.Fatalf("empty upstream provider set must reach the card: %s", got.Body.String())
	}
	if got := get(second); !strings.Contains(got.Body.String(), `"needs_token"`) {
		t.Fatalf("second session saw quota: %s", got.Body.String())
	}
	for _, sealed := range stub.quotaTokens {
		if bytes.Contains(sealed, []byte(quotaReadToken)) {
			t.Fatal("token stored in plaintext")
		}
	}
	if got := serve(app, authed(http.MethodPost, "/hq/logout", "", first)); got.Code != 204 {
		t.Fatalf("logout: %d", got.Code)
	}
	if len(stub.quotaTokens) != 0 {
		t.Fatal("logout left a sealed token")
	}
}

func TestQuotaRejectsBadTokensAndNeverSavesThem(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()
	app, stub := newHQApp(t)
	app.quotaURL = upstream.URL
	cookie := login(t, app)
	for _, body := range []string{`{"token":""}`, `{"token":"` + strings.Repeat("x", 513) + `"}`, `{"token":"x"}{}`} {
		if got := serve(app, authed(http.MethodPut, "/hq/api/quota/token", body, cookie)); got.Code != 400 {
			t.Fatalf("invalid body accepted: %d", got.Code)
		}
	}
	if got := serve(app, authed(http.MethodPut, "/hq/api/quota/token", `{"token":"bad"}`, cookie)); got.Code != 401 || !strings.Contains(got.Body.String(), "token not accepted") {
		t.Fatalf("rejected token: %d %s", got.Code, got.Body.String())
	}
	if len(stub.quotaTokens) != 0 {
		t.Fatal("rejected token stored")
	}
}

func TestQuotaUpstreamFailureDoesNotBreakHQOrLeakToken(t *testing.T) {
	mode := "ok"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "reject":
			w.WriteHeader(http.StatusUnauthorized)
		case "broken":
			_, _ = io.WriteString(w, "not json")
		default:
			_, _ = io.WriteString(w, quotaReply)
		}
	}))
	defer upstream.Close()
	app, stub := newHQApp(t)
	app.quotaURL = upstream.URL
	cookie := login(t, app)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	serve(app, authed(http.MethodPut, "/hq/api/quota/token", `{"token":"`+quotaReadToken+`"}`, cookie))
	mode = "broken"
	if got := serve(app, authed(http.MethodGet, "/hq/api/quota", "", cookie)); got.Code != 200 || !strings.Contains(got.Body.String(), `"unavailable"`) {
		t.Fatalf("malformed upstream: %d %s", got.Code, got.Body.String())
	}
	upstream.Close()
	if got := serve(app, authed(http.MethodGet, "/hq/api/quota", "", cookie)); got.Code != 200 || !strings.Contains(got.Body.String(), `"unavailable"`) {
		t.Fatalf("offline upstream: %d %s", got.Code, got.Body.String())
	}
	if got := serve(app, authed(http.MethodPut, "/hq/api/quota/token", `{"token":"`+quotaReadToken+`"}`, cookie)); got.Code != 502 {
		t.Fatalf("offline token verify: %d %s", got.Code, got.Body.String())
	}
	if strings.Contains(logs.String(), quotaReadToken) {
		t.Fatal("token appeared in logs")
	}
	if len(stub.quotaTokens) != 1 {
		t.Fatal("temporary outage deleted existing token")
	}
	if got := serve(app, authed(http.MethodGet, "/hq/api/state", "", cookie)); got.Code != 200 {
		t.Fatalf("HQ state after outage: %d", got.Code)
	}
}

func TestQuotaRejectedStoredTokenIsDeleted(t *testing.T) {
	mode := http.StatusOK
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(mode)
		if mode == http.StatusOK {
			_, _ = io.WriteString(w, quotaReply)
		}
	}))
	defer upstream.Close()
	app, stub := newHQApp(t)
	app.quotaURL = upstream.URL
	cookie := login(t, app)
	serve(app, authed(http.MethodPut, "/hq/api/quota/token", `{"token":"`+quotaReadToken+`"}`, cookie))
	mode = http.StatusUnauthorized
	got := serve(app, authed(http.MethodGet, "/hq/api/quota", "", cookie))
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"reason":"rejected"`) || len(stub.quotaTokens) != 0 {
		t.Fatalf("rejected stored token: %d %s", got.Code, got.Body.String())
	}
}

func TestQuotaSealedBlobCannotMoveBetweenSessions(t *testing.T) {
	app, stub := newHQApp(t)
	first := login(t, app)
	second := login(t, app)
	sealed, err := app.sealQuotaToken(hashHex(first.Value), quotaReadToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := stub.PutQuotaToken(context.Background(), hashHex(second.Value), sealed); err != nil {
		t.Fatal(err)
	}
	got := serve(app, authed(http.MethodGet, "/hq/api/quota", "", second))
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"needs_token"`) || len(stub.quotaTokens) != 0 {
		t.Fatalf("moved blob accepted: %d %s", got.Code, got.Body.String())
	}
}

func TestQuotaNeverFollowsAnUpstreamRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer upstream.Close()
	app, _ := newHQApp(t)
	app.quotaURL = upstream.URL
	cookie := login(t, app)
	got := serve(app, authed(http.MethodPut, "/hq/api/quota/token", `{"token":"`+quotaReadToken+`"}`, cookie))
	if got.Code != 502 || redirected {
		t.Fatalf("redirect followed: %d, target hit %v", got.Code, redirected)
	}
}

func TestQuotaEndpointsRequireSession(t *testing.T) {
	app, _ := newHQApp(t)
	for _, methodPath := range []struct{ method, path string }{
		{http.MethodGet, "/hq/api/quota"},
		{http.MethodPut, "/hq/api/quota/token"},
		{http.MethodDelete, "/hq/api/quota/token"},
	} {
		if got := serve(app, httptest.NewRequest(methodPath.method, methodPath.path, nil)); got.Code != 401 {
			t.Fatalf("%s %s: %d", methodPath.method, methodPath.path, got.Code)
		}
	}
}
