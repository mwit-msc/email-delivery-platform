package edp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOIDCIdentityAndRefresh(t *testing.T) {
	testOIDCIdentityAndRefresh(t, false)
}

func TestOIDCRuntimeConfiguration(t *testing.T) {
	testOIDCIdentityAndRefresh(t, true)
}

func testOIDCIdentityAndRefresh(t *testing.T, dynamic bool) {
	t.Helper()
	a := testApp(t)
	ctx := context.Background()
	groupsClaim := "groups"
	if dynamic {
		groupsClaim = "member_of"
	}
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	var server *httptest.Server
	var expectedNonce, challenge string
	var refreshCalls atomic.Int64
	var userInfoFail atomic.Bool
	sign := func(nonce string) string {
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key"})
		claims, _ := json.Marshal(map[string]any{"iss": server.URL, "aud": "edp-test", "sub": "pairwise-person", "nonce": nonce, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
		input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		hash := sha256.Sum256([]byte(input))
		signature, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
		return input + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "userinfo_endpoint": server.URL + "/userinfo", "jwks_uri": server.URL + "/jwks", "response_types_supported": []string{"code"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"keys": []any{map[string]string{"kty": "RSA", "alg": "RS256", "kid": "test-key", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		client, secret, basic := r.BasicAuth()
		if !basic {
			client, secret = r.FormValue("client_id"), r.FormValue("client_secret")
		}
		if client != "edp-test" || secret != "test-secret" {
			problem(w, 401, "client mismatch")
			return
		}
		if r.FormValue("grant_type") == "refresh_token" {
			refreshCalls.Add(1)
			respond(w, 200, map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600, "token_type": "Bearer"})
			return
		}
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			problem(w, 400, "PKCE mismatch")
			return
		}
		respond(w, 200, map[string]any{"access_token": "initial-access", "refresh_token": "initial-refresh", "expires_in": 3600, "token_type": "Bearer", "id_token": sign(expectedNonce)})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if userInfoFail.Load() {
			problem(w, 503, "temporary failure")
			return
		}
		respond(w, 200, map[string]any{"sub": "pairwise-person", "name": "SSO Person", groupsClaim: []string{"organizers"}})
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	if dynamic {
		if a.C.Issuer != "" || a.C.ClientID != "" {
			t.Fatal("runtime configuration test requires blank bootstrap identity")
		}
		c := a.C
		c.Issuer, c.ClientID, c.ClientSecret = server.URL, "edp-test", "test-secret"
		c.PublicURL, c.ProviderName = "http://runtime.example.test", "Configured SSO"
		c.OIDCScopes, c.GroupsClaim = []string{"openid", "profile", "membership", "offline_access"}, groupsClaim
		if e = a.saveRuntime(ctx, "oidc", c); e != nil {
			t.Fatal("save runtime OIDC configuration", e)
		}
		login := httptest.NewRecorder()
		a.login(login, httptest.NewRequest("GET", "/login", nil))
		if login.Code != 200 || !strings.Contains(login.Body.String(), "/auth/sso") {
			t.Fatal("runtime SSO was not offered on login", login.Code)
		}
	} else {
		a.C.Issuer = server.URL
		a.C.ClientID = "edp-test"
		a.C.ClientSecret = "test-secret"
	}
	r := httptest.NewRequest("GET", "/auth/sso", nil)
	w := httptest.NewRecorder()
	a.ssoStart(w, r)
	if w.Code != 302 {
		t.Fatalf("SSO start %d %s", w.Code, w.Body)
	}
	location, _ := url.Parse(w.Header().Get("Location"))
	if location.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("PKCE missing")
	}
	if dynamic {
		if location.Host != strings.TrimPrefix(server.URL, "http://") || location.Query().Get("client_id") != "edp-test" || location.Query().Get("scope") != "openid profile membership offline_access" || location.Query().Get("redirect_uri") != "http://runtime.example.test/auth/callback" {
			t.Fatal("authorization request did not use runtime issuer/client/scopes/URL")
		}
	}
	expectedNonce = location.Query().Get("nonce")
	challenge = location.Query().Get("code_challenge")
	state := location.Query().Get("state")
	callback := httptest.NewRequest("GET", "/auth/callback?code=valid&state="+state, nil)
	for _, cookie := range w.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	a.ssoCallback(w, callback)
	if w.Code != 303 {
		t.Fatalf("SSO callback %d %s", w.Code, w.Body)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "edp_session" {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("SSO session missing")
	}
	var uid string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE oidc_subject='pairwise-person' AND oidc_issuer=$1", server.URL).Scan(&uid)
	if uid == "" {
		t.Fatal("pairwise identity not persisted")
	}
	a.DB.Exec(ctx, "UPDATE users SET groups_expires_at=now()-interval '1 minute' WHERE id=$1", uid)
	a.DB.Exec(ctx, "UPDATE sessions SET token_expiry=now()-interval '1 minute' WHERE token_hash=$1", tokenHash(sessionCookie.Value))
	userInfoFail.Store(true)
	p := &Principal{ID: uid, Method: "sso"}
	if e = a.refreshIdentity(ctx, p, tokenHash(sessionCookie.Value)); e == nil {
		t.Fatal("failed UserInfo marked fresh")
	}
	var encoded string
	a.DB.QueryRow(ctx, "SELECT refresh_token FROM sessions WHERE token_hash=$1", tokenHash(sessionCookie.Value)).Scan(&encoded)
	saved, _ := unseal(a.C.Secret, encoded)
	if saved != "new-refresh" || p.GroupsFresh {
		t.Fatal("rotation successor lost after UserInfo failure")
	}
	userInfoFail.Store(false)
	if e = a.refreshIdentity(ctx, p, tokenHash(sessionCookie.Value)); e != nil || !p.GroupsFresh || !contains(p.Groups, "organizers") {
		t.Fatal("group refresh failed", e)
	}
	if refreshCalls.Load() != 1 {
		t.Fatal("spent refresh token retried", refreshCalls.Load())
	}
	bad := httptest.NewRequest("GET", "/auth/callback?code=valid&state=wrong", nil)
	bad.AddCookie(sessionCookie)
	w = httptest.NewRecorder()
	a.ssoCallback(w, bad)
	if w.Code != 400 {
		t.Fatal("invalid state accepted")
	}
	if dynamic {
		if a.C.Issuer != "" || a.C.ClientID != "" {
			t.Fatal("runtime authentication mutated bootstrap configuration")
		}
		return
	}
	// Configured but unavailable SSO must not prevent local recovery login.
	a.C.Issuer = "http://127.0.0.1:1"
	a.C.RootPassword = "test-root-password"
	w = httptest.NewRecorder()
	a.ssoStart(w, httptest.NewRequest("GET", "/auth/sso", nil))
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "SSO+unavailable") {
		t.Fatal("SSO outage did not offer fallback")
	}
}
