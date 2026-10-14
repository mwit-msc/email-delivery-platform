package edp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type googleTestTransport func(*http.Request) (*http.Response, error)

func (f googleTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func googleAccountRequest(a *App, cookie *http.Cookie, method, path string, values url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func TestGoogleApplicationAndPersonalPermissions(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	root := runtimeTestSession(t, a, true)
	user := runtimeTestSession(t, a, false)
	settings := url.Values{"action": {"google.save"}, "enabled": {"on"}, "client_id": {"synthetic-client"}, "client_secret": {"synthetic-secret"}}
	if w := runtimeSettingsRequest(a, user, settings, true); w.Code != 403 {
		t.Fatal("application admin configured platform OAuth", w.Code)
	}
	if w := runtimeSettingsRequest(a, root, settings, false); w.Code != 403 {
		t.Fatal("settings missing CSRF", w.Code)
	}
	if w := runtimeSettingsRequest(a, root, settings, true); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	c, err := a.runtimeConfig(ctx)
	if err != nil || !c.googleEnabled() || c.GoogleClientSecret != "synthetic-secret" {
		t.Fatal(c.googleEnabled(), err)
	}
	if a.C.GoogleClientSecret != "" {
		t.Fatal("bootstrap config mutated")
	}
	var raw string
	a.DB.QueryRow(ctx, "SELECT runtime_config::text FROM platform_settings WHERE id=1").Scan(&raw)
	if strings.Contains(raw, "synthetic-secret") {
		t.Fatal("plaintext client secret")
	}
	settings.Set("client_secret", "")
	if w := runtimeSettingsRequest(a, root, settings, true); w.Code != 303 {
		t.Fatal(w.Code)
	}
	c, _ = a.runtimeConfig(ctx)
	if c.GoogleClientSecret != "synthetic-secret" {
		t.Fatal("blank erased secret")
	}
	if w := googleAccountRequest(a, user, "GET", "/account/connections", nil); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self' https://accounts.google.com") || !strings.Contains(w.Body.String(), "Connect Google Drive") || strings.Contains(w.Body.String(), "synthetic-secret") {
		t.Fatal("personal page", w.Code, w.Body)
	}
	if w := googleAccountRequest(a, user, "POST", "/account/google/connect", nil); w.Code != 403 {
		t.Fatal("connect CSRF", w.Code)
	}
	w := googleAccountRequest(a, user, "POST", "/account/google/connect", url.Values{"csrf": {"runtime-csrf"}, "return_to": {"https://evil.example/test"}})
	u, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != 303 || u.Host != "accounts.google.com" || u.Query().Get("access_type") != "offline" || u.Query().Get("code_challenge_method") != "S256" || !contains(strings.Fields(u.Query().Get("scope")), googleDriveScope) {
		t.Fatal("consent request", w.Code)
	}
	state := u.Query().Get("state")
	var returnTo string
	a.DB.QueryRow(ctx, "SELECT return_to FROM google_oauth_states WHERE state_hash=$1", tokenHash(state)).Scan(&returnTo)
	if returnTo != "/account/connections" {
		t.Fatal("open redirect")
	}
	if w = googleAccountRequest(a, root, "GET", "/account/google/callback?state="+state+"&error=access_denied", nil); w.Code != 400 {
		t.Fatal("cross-user callback accepted", w.Code)
	}
	other := runtimeTestSession(t, a, false)
	if w = googleAccountRequest(a, other, "GET", "/account/google/callback?state="+state+"&error=access_denied", nil); w.Code != 400 {
		t.Fatal("wrong session")
	}
	if w = googleAccountRequest(a, user, "GET", "/account/google/callback?state="+state+"&error=access_denied", nil); w.Code != 303 {
		t.Fatal("denied consent", w.Code)
	}
	settings.Set("enabled", "")
	settings.Set("client_secret_clear", "on")
	if w = runtimeSettingsRequest(a, root, settings, true); w.Code != 303 {
		t.Fatal(w.Code)
	}
	c, _ = a.runtimeConfig(ctx)
	if c.googleEnabled() || c.GoogleClientSecret != "" {
		t.Fatal("clear did not disable")
	}
}

func TestGooglePersonalConsentAndRefresh(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	// A single connection exposes nested pool acquisition deterministically.
	// Refresh must read runtime settings through its existing transaction.
	poolConfig, err := pgxpool.ParseConfig(a.C.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.MaxConns = 1
	limited, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	a.DB.Close()
	a.DB = limited

	user := runtimeTestSession(t, a, false)
	second := runtimeTestSession(t, a, false)
	a.C.GoogleClientID = "synthetic-client"
	a.C.GoogleClientSecret = "synthetic-secret"
	var uid, otherID string
	a.DB.QueryRow(ctx, "SELECT user_id FROM sessions WHERE token_hash=$1", tokenHash(user.Value)).Scan(&uid)
	a.DB.QueryRow(ctx, "SELECT user_id FROM sessions WHERE token_hash=$1", tokenHash(second.Value)).Scan(&otherID)
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var nonce, challenge string
	var refreshCalls atomic.Int64
	var badNonce, partialConsent atomic.Bool
	sign := func() string {
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "synthetic"})
		n := nonce
		if badNonce.Load() {
			n = "wrong"
		}
		claims, _ := json.Marshal(map[string]any{"iss": googleIssuer, "aud": a.C.GoogleClientID, "sub": "google-subject-one", "email": "alice@example.test", "email_verified": true, "nonce": n, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
		input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		sum := sha256.Sum256([]byte(input))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, sum[:])
		return input + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	original := http.DefaultTransport
	http.DefaultTransport = googleTestTransport(func(r *http.Request) (*http.Response, error) {
		out := httptest.NewRecorder()
		switch r.URL.Host + r.URL.Path {
		case "oauth2.googleapis.com/token":
			r.ParseForm()
			if r.FormValue("grant_type") == "refresh_token" {
				refreshCalls.Add(1)
				if r.FormValue("refresh_token") != "initial-refresh" {
					t.Error("spent refresh token retried")
				}
				respond(out, 200, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
				break
			}
			sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				t.Error("PKCE verifier")
			}
			scope := googleDriveScope
			if partialConsent.Load() {
				scope = "openid email"
			}
			respond(out, 200, map[string]any{"access_token": "initial-access", "refresh_token": "initial-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": scope, "id_token": sign()})
		case "accounts.google.com/.well-known/openid-configuration":
			respond(out, 200, map[string]any{"issuer": googleIssuer, "authorization_endpoint": googleIssuer + "/o/oauth2/auth", "token_endpoint": "https://oauth2.googleapis.com/token", "jwks_uri": "https://www.googleapis.com/oauth2/v3/certs", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "www.googleapis.com/oauth2/v3/certs":
			respond(out, 200, map[string]any{"keys": []any{map[string]string{"kty": "RSA", "alg": "RS256", "kid": "synthetic", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(private.N.Bytes()), "e": "AQAB"}}})
		case "www.googleapis.com/drive/v3/files":
			if r.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Error("wrong user's access token")
			}
			var stored string
			a.DB.QueryRow(ctx, "SELECT refresh_enc FROM google_connections WHERE user_id=$1", uid).Scan(&stored)
			refresh, _ := unseal(a.C.Secret, stored)
			if refresh != "rotated-refresh" {
				t.Error("request preceded persistence of rotated token")
			}
			respond(out, 200, map[string]any{"files": []drivePDF{{ID: "synthetic-pdf", Name: "Alice.pdf"}}})
		case "www.googleapis.com/drive/v3/files/synthetic-pdf":
			if r.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Error("background scan used another account")
			}
			out.Header().Set("Content-Type", "application/pdf")
			out.Write(textCertificatePDF())
		case "sheets.googleapis.com/v4/spreadsheets/synthetic-sheet":
			if r.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Error("sheet did not use personal credentials")
			}
			respond(out, 200, map[string]any{"sheets": []any{map[string]any{"properties": map[string]any{"sheetId": 0, "title": "Recipients", "gridProperties": map[string]int{"rowCount": 2, "columnCount": 2}}}}})
		default:
			if r.URL.Host == "sheets.googleapis.com" && strings.Contains(r.URL.Path, "/values/") {
				respond(out, 200, map[string]any{"values": [][]string{{"email", "name"}, {"alice@example.test", "Alice"}}})
			} else {
				return nil, fmt.Errorf("unexpected Google endpoint: %s", r.URL.Path)
			}
		}
		return out.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	start := func() string {
		t.Helper()
		w := googleAccountRequest(a, user, "POST", "/account/google/connect", url.Values{"csrf": {"runtime-csrf"}, "return_to": {"/campaigns/synthetic/sheet?tab=review"}})
		if w.Code != 303 {
			t.Fatal(w.Code, w.Body)
		}
		u, _ := url.Parse(w.Header().Get("Location"))
		nonce, challenge = u.Query().Get("nonce"), u.Query().Get("code_challenge")
		return u.Query().Get("state")
	}
	state := start()
	badNonce.Store(true)
	if w := googleAccountRequest(a, user, "GET", "/account/google/callback?code=synthetic&state="+state, nil); w.Code != 400 {
		t.Fatal("bad nonce accepted", w.Code, w.Body)
	}
	badNonce.Store(false)
	state = start()
	partialConsent.Store(true)
	if w := googleAccountRequest(a, user, "GET", "/account/google/callback?code=synthetic&state="+state, nil); w.Code != 400 {
		t.Fatal("partial consent accepted", w.Code)
	}
	partialConsent.Store(false)
	state = start()
	w := googleAccountRequest(a, user, "GET", "/account/google/callback?code=synthetic&state="+state, nil)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	if w = googleAccountRequest(a, user, "GET", "/account/google/callback?code=synthetic&state="+state, nil); w.Code != 400 {
		t.Fatal("replay accepted")
	}
	owner, connection, err := a.googleReadIdentity(ctx, uid)
	if err != nil || owner != uid || connection == "" {
		t.Fatal("personal account not persisted", err)
	}
	var access, refresh string
	a.DB.QueryRow(ctx, "SELECT access_enc,refresh_enc FROM google_connections WHERE user_id=$1", uid).Scan(&access, &refresh)
	if access == "initial-access" || refresh == "initial-refresh" {
		t.Fatal("plaintext credentials")
	}
	if u, _, err := a.googleReadIdentity(ctx, otherID); err != nil || u != "" {
		t.Fatal("other user inherited connection")
	}
	var identityCount int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM users WHERE oidc_subject='google-subject-one'").Scan(&identityCount)
	if identityCount != 0 {
		t.Fatal("connection mutated SSO identity")
	}
	a.DB.Exec(ctx, "UPDATE google_connections SET token_expiry=now()-interval '1 minute' WHERE user_id=$1", uid)
	var wg sync.WaitGroup
	tokenCtx, cancelTokens := context.WithTimeout(ctx, 5*time.Second)
	defer cancelTokens()
	var tokenFailed atomic.Bool
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := (googleUserTokens{a, tokenCtx, uid, connection}).Token()
			if err != nil || tok.AccessToken != "rotated-access" {
				tokenFailed.Store(true)
				t.Error("refresh", err)
			}
		}()
	}
	wg.Wait()
	if tokenFailed.Load() {
		t.Fatal("concurrent token refresh failed with a single-connection pool")
	}
	if refreshCalls.Load() != 1 {
		t.Fatal("refresh was not serialized", refreshCalls.Load())
	}
	files, err := a.drivePDFs(ctx, "https://drive.google.com/drive/folders/synthetic-folder", owner, connection)
	if err != nil || len(files) != 1 {
		t.Fatal("personal Drive listing", err)
	}
	// A scan freezes the person's grant and the worker uses that grant locally.
	if extractionCapabilities(ctx) == nil {
		campaign := newID("cmp_")
		_, err = a.DB.Exec(ctx, "INSERT INTO campaigns(id,application_id,name,kind,state,source_rows,created_by) VALUES($1,'app_demo','Personal scan','certificate','draft',$2,$3)", campaign, [][]string{{"name", "url"}, {"Alice Example", ""}}, uid)
		if err != nil {
			t.Fatal(err)
		}
		links := certificateLinks{GoogleUser: owner, GoogleConnection: connection, Files: files, Columns: []int{0}, URLColumn: 1, Actor: uid}
		tx, err := a.DB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = queueCertificateScan(ctx, tx, campaign, &links); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		data, _ := json.Marshal(links)
		_, err = tx.Exec(ctx, "UPDATE campaigns SET certificate_links=$2 WHERE id=$1", campaign, data)
		if err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		job, err := a.claimCertificateScan(ctx)
		if err != nil || job == nil || job.GoogleUser != uid || job.GoogleConnection != connection {
			t.Fatal("scan identity not frozen", job, err)
		}
		a.processCertificateScan(ctx, *job)
		var state, text string
		a.DB.QueryRow(ctx, "SELECT state,text_enc FROM certificate_scan_files WHERE id=$1", job.ID).Scan(&state, &text)
		plain, _ := unseal(a.C.Secret, text)
		if state != "done" || !strings.Contains(plain, "Alice Example") {
			t.Fatal("personal PDF scan", state)
		}
	}
	rows, err := a.sheetRowsForUser(ctx, "https://docs.google.com/spreadsheets/d/synthetic-sheet/edit", uid)
	if err != nil || len(rows) != 2 {
		t.Fatal("personal sheet import", err)
	}
	c := a.C
	c.GoogleClientSecret = "replacement-secret"
	if err = a.saveRuntime(ctx, "google", c); err != nil {
		t.Fatal(err)
	}
	if _, err = (googleUserTokens{a, ctx, uid, connection}).Token(); err == nil {
		t.Fatal("configuration change reused old grant")
	}
	if err = a.saveRuntime(ctx, "google", a.C); err != nil {
		t.Fatal(err)
	}
	_, err = a.DB.Exec(ctx, "INSERT INTO google_connections(user_id,id,subject,email,config_hash,access_enc,refresh_enc,token_expiry) VALUES($1,'other-grant','other-subject','bob@example.test',$2,$3,$4,now()+interval '1 hour')", otherID, googleConfigHash(a.C), seal(a.C.Secret, "other-access"), seal(a.C.Secret, "other-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	state = start()
	if w = googleAccountRequest(a, user, "POST", "/account/google/disconnect", url.Values{"csrf": {"runtime-csrf"}, "user_id": {otherID}}); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	var otherRemaining int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM google_connections WHERE user_id=$1", otherID).Scan(&otherRemaining)
	if otherRemaining != 1 {
		t.Fatal("disconnect changed another user's connection")
	}
	if _, err = (googleUserTokens{a, ctx, uid, connection}).Token(); err == nil {
		t.Fatal("disconnected scan still acquired credentials")
	}
	if w = googleAccountRequest(a, user, "GET", "/account/google/callback?code=synthetic&state="+state, nil); w.Code != 400 {
		t.Fatal("disconnect left pending flow")
	}
	// A new connection must not revive jobs frozen against the old connection ID.
	state = start()
	if w = googleAccountRequest(a, user, "GET", "/account/google/callback?code=synthetic&state="+state, nil); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	if _, err = (googleUserTokens{a, ctx, uid, connection}).Token(); err == nil {
		t.Fatal("old scan switched accounts after reconnect")
	}
	// Existing service-account API paths never pick up a personal connection.
	if _, err = a.sheetRows(ctx, "https://docs.google.com/spreadsheets/d/synthetic-sheet/edit"); err == nil {
		t.Fatal("machine import inherited user connection")
	}
}

func TestGoogleReturnDestinations(t *testing.T) {
	for _, s := range []string{"https://evil.example/", "//evil.example/", "/account/google/connect", "/campaigns/../settings"} {
		if googleReturnTo(s) != "/account/connections" {
			t.Fatal(s)
		}
	}
	if googleReturnTo("/campaigns/cmp_synthetic/sheet?tab=review") != "/campaigns/cmp_synthetic/sheet?tab=review" {
		t.Fatal("campaign return lost")
	}
}
