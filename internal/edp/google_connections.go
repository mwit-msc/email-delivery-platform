package edp

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const googleDriveScope = "https://www.googleapis.com/auth/drive.readonly"
const googleIssuer = "https://accounts.google.com"

// Google consent grants belong to an authenticated EDP user, independently of
// their login method. A Google subject is never used to discover an EDP identity.
func googleOAuthConfig(c Config) *oauth2.Config {
	return &oauth2.Config{ClientID: c.GoogleClientID, ClientSecret: c.GoogleClientSecret, Endpoint: google.Endpoint, RedirectURL: c.PublicURL + "/account/google/callback", Scopes: []string{"openid", "email", googleDriveScope}}
}
func googleConfigHash(c Config) string {
	return tokenHash(c.GoogleClientID + "\x00" + c.GoogleClientSecret + "\x00" + c.PublicURL + "\x00" + fmt.Sprint(c.GoogleOAuthDisabled))
}
func googleReturnTo(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return "/account/connections"
	}
	if u.Path == "/account/connections" {
		return u.String()
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) >= 3 && len(parts) <= 4 && parts[1] == "campaigns" && googleFileID.MatchString(parts[2]) && (len(parts) == 3 || parts[3] == "sheet") {
		return u.String()
	}
	return "/account/connections"
}
func googleResultURL(path, status string) string {
	u, _ := url.Parse(googleReturnTo(path))
	q := u.Query()
	q.Set("google", status)
	u.RawQuery = q.Encode()
	return u.String()
}
func (a *App) googleAccountUser(w http.ResponseWriter, r *http.Request) bool {
	p := person(r)
	if p == nil {
		http.Redirect(w, r, "/login", 303)
		return false
	}
	if p.KeyID != "" {
		problem(w, 403, "connect Google using your personal EDP session")
		return false
	}
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			problem(w, 400, "invalid form")
			return false
		}
		if subtle.ConstantTimeCompare([]byte(p.CSRF), []byte(r.FormValue("csrf"))) != 1 {
			problem(w, 403, "invalid CSRF token")
			return false
		}
	}
	return true
}
func (a *App) googleConnectionView(ctx context.Context, user string) map[string]any {
	c, err := a.runtimeConfig(ctx)
	if err != nil {
		return map[string]any{"Unavailable": true}
	}
	view := map[string]any{"Configured": c.googleEnabled(), "ServiceAccount": c.GoogleCredentials != "", "CanRead": c.GoogleCredentials != ""}
	var email, fingerprint string
	err = a.DB.QueryRow(ctx, "SELECT email,config_hash FROM google_connections WHERE user_id=$1", user).Scan(&email, &fingerprint)
	if err == nil {
		valid := c.googleEnabled() && fingerprint == googleConfigHash(c)
		view["Email"] = email
		view["Connected"] = valid
		view["Stale"] = !valid
		view["HasConnection"] = true
		view["CanRead"] = valid
	} else if err != pgx.ErrNoRows {
		view["Unavailable"] = true
		view["CanRead"] = false
	}
	return view
}
func (a *App) googleConnections(w http.ResponseWriter, r *http.Request) {
	if !a.googleAccountUser(w, r) {
		return
	}
	a.render(w, r, "google-connections", map[string]any{"Google": a.googleConnectionView(r.Context(), person(r).ID), "GoogleResult": r.URL.Query().Get("google")})
}
func (a *App) googleConnect(w http.ResponseWriter, r *http.Request) {
	if !a.googleAccountUser(w, r) {
		return
	}
	c, err := a.runtimeConfig(r.Context())
	if err != nil || !c.googleEnabled() {
		problem(w, 400, "ask a platform administrator to configure Google account connections")
		return
	}
	cookie, err := r.Cookie("edp_session")
	if err != nil {
		problem(w, 401, "personal session required")
		return
	}
	state, nonce, verifier := newID("google_"), newID("nonce_"), oauth2.GenerateVerifier()
	tx, err := a.DB.Begin(r.Context())
	if err != nil {
		problem(w, 503, "Google connection unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize connect/disconnect for this person and keep a single pending flow.
	var disabled bool
	err = tx.QueryRow(r.Context(), "SELECT disabled FROM users WHERE id=$1 FOR UPDATE", person(r).ID).Scan(&disabled)
	if err == nil && !disabled {
		_, err = tx.Exec(r.Context(), "DELETE FROM google_oauth_states WHERE user_id=$1 OR expires_at<now()", person(r).ID)
	} else {
		problem(w, 403, "account unavailable")
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO google_oauth_states(state_hash,user_id,session_hash,verifier_enc,nonce,config_hash,return_to,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes')", tokenHash(state), person(r).ID, tokenHash(cookie.Value), seal(a.C.Secret, verifier), nonce, googleConfigHash(c), googleReturnTo(r.FormValue("return_to")))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		problem(w, 503, "Google connection unavailable")
		return
	}
	http.Redirect(w, r, googleOAuthConfig(c).AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent select_account"), oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), 303)
}
func (a *App) googleCallback(w http.ResponseWriter, r *http.Request) {
	if !a.googleAccountUser(w, r) {
		return
	}
	cookie, err := r.Cookie("edp_session")
	if err != nil {
		problem(w, 400, "Google connection session expired")
		return
	}
	stateHash := tokenHash(r.URL.Query().Get("state"))
	sessionHash := tokenHash(cookie.Value)
	var encrypted, nonce, fingerprint, returnTo string
	err = a.DB.QueryRow(r.Context(), "SELECT verifier_enc,nonce,config_hash,return_to FROM google_oauth_states WHERE state_hash=$1 AND user_id=$2 AND session_hash=$3 AND expires_at>now()", stateHash, person(r).ID, sessionHash).Scan(&encrypted, &nonce, &fingerprint, &returnTo)
	if err != nil {
		problem(w, 400, "Google connection expired or belongs to another session; connect again")
		return
	}
	if r.URL.Query().Get("error") != "" {
		a.DB.Exec(r.Context(), "DELETE FROM google_oauth_states WHERE state_hash=$1", stateHash)
		http.Redirect(w, r, googleResultURL(returnTo, "cancelled"), 303)
		return
	}
	c, err := a.runtimeConfig(r.Context())
	if err != nil || !c.googleEnabled() || googleConfigHash(c) != fingerprint {
		problem(w, 400, "Google application settings changed; connect again")
		return
	}
	verifier, err := unseal(a.C.Secret, encrypted)
	if err != nil {
		problem(w, 503, "Google connection unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 20 * time.Second})
	conf := googleOAuthConfig(c)
	tok, err := conf.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		problem(w, 400, "Google authorization failed; connect again")
		return
	}
	if !contains(strings.Fields(fmt.Sprint(tok.Extra("scope"))), googleDriveScope) || tok.RefreshToken == "" {
		problem(w, 400, "allow read-only Drive access to enable background scans; connect again")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	provider, err := oidc.NewProvider(ctx, googleIssuer)
	if err != nil {
		problem(w, 503, "Google identity verification unavailable")
		return
	}
	identity, err := provider.Verifier(&oidc.Config{ClientID: c.GoogleClientID}).Verify(ctx, rawID)
	if err != nil || identity.Nonce != nonce || identity.Subject == "" {
		problem(w, 400, "Google identity verification failed; connect again")
		return
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err = identity.Claims(&claims); err != nil || !claims.Verified || claims.Email == "" || len(claims.Email) > 320 {
		problem(w, 400, "Google account must have a verified email address")
		return
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		problem(w, 503, "Google connection unavailable")
		return
	}
	defer tx.Rollback(ctx)
	// Recheck configuration and the one-time state after network calls. Disconnect
	// removes pending states, preventing an in-flight callback from reconnecting.
	var disabled bool
	err = tx.QueryRow(ctx, "SELECT disabled FROM users WHERE id=$1 FOR UPDATE", person(r).ID).Scan(&disabled)
	if err != nil || disabled {
		problem(w, 403, "account unavailable")
		return
	}
	var configData []byte
	if err = tx.QueryRow(ctx, "SELECT runtime_config FROM platform_settings WHERE id=1 FOR SHARE").Scan(&configData); err != nil {
		problem(w, 503, "Google configuration unavailable")
		return
	}
	fresh, err := readRuntimeConfig(ctx, tx, a.C)
	if err != nil || !fresh.googleEnabled() || googleConfigHash(fresh) != fingerprint {
		problem(w, 400, "Google application settings changed; connect again")
		return
	}
	tag, err := tx.Exec(ctx, "DELETE FROM google_oauth_states g USING sessions s WHERE g.state_hash=$1 AND g.user_id=$2 AND g.session_hash=$3 AND g.expires_at>now() AND s.token_hash=g.session_hash AND s.expires_at>now()", stateHash, person(r).ID, sessionHash)
	if err != nil || tag.RowsAffected() != 1 {
		problem(w, 400, "Google connection was cancelled or expired; connect again")
		return
	}
	_, err = tx.Exec(ctx, `INSERT INTO google_connections(user_id,id,subject,email,config_hash,access_enc,refresh_enc,token_expiry) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(user_id) DO UPDATE SET id=excluded.id,subject=excluded.subject,email=excluded.email,config_hash=excluded.config_hash,access_enc=excluded.access_enc,refresh_enc=excluded.refresh_enc,token_expiry=excluded.token_expiry,connected_at=now()`, person(r).ID, newID("gconn_"), identity.Subject, claims.Email, fingerprint, seal(a.C.Secret, tok.AccessToken), seal(a.C.Secret, tok.RefreshToken), tok.Expiry)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		problem(w, 503, "Google connection could not be saved")
		return
	}
	a.audit(r.Context(), person(r).ID, "account.google.connected", "*", "Read-only Drive access")
	http.Redirect(w, r, googleResultURL(returnTo, "connected"), 303)
}
func (a *App) googleDisconnect(w http.ResponseWriter, r *http.Request) {
	if !a.googleAccountUser(w, r) {
		return
	}
	tx, err := a.DB.Begin(r.Context())
	if err != nil {
		problem(w, 503, "Google connection unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	err = tx.QueryRow(r.Context(), "SELECT id FROM users WHERE id=$1 FOR UPDATE", person(r).ID).Scan(&id)
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM google_oauth_states WHERE user_id=$1", id)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM google_connections WHERE user_id=$1", id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		problem(w, 503, "Google connection could not be removed")
		return
	}
	a.audit(r.Context(), id, "account.google.disconnected", "*", "")
	http.Redirect(w, r, googleResultURL(r.FormValue("return_to"), "disconnected"), 303)
}

// Resolve only this person's connection. Machine/service-account imports never
// inherit a human's tokens. Frozen scan IDs cannot silently switch accounts.
func (a *App) googleReadIdentity(ctx context.Context, user string) (string, string, error) {
	if user == "" {
		return "", "", nil
	}
	var id, hash string
	err := a.DB.QueryRow(ctx, "SELECT id,config_hash FROM google_connections WHERE user_id=$1", user).Scan(&id, &hash)
	if err == pgx.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("Google connection unavailable")
	}
	c, err := a.runtimeConfig(ctx)
	if err != nil || !c.googleEnabled() || hash != googleConfigHash(c) {
		return "", "", fmt.Errorf("reconnect your Google account after the application settings change")
	}
	return user, id, nil
}

type googleUserTokens struct {
	app              *App
	ctx              context.Context
	user, connection string
}

func (s googleUserTokens) Token() (*oauth2.Token, error) {
	tx, err := s.app.DB.Begin(s.ctx)
	if err != nil {
		return nil, fmt.Errorf("Google connection unavailable")
	}
	defer tx.Rollback(s.ctx)
	var access, refresh, hash string
	var expiry time.Time
	err = tx.QueryRow(s.ctx, `SELECT g.access_enc,g.refresh_enc,g.token_expiry,g.config_hash FROM google_connections g JOIN users u ON u.id=g.user_id WHERE g.user_id=$1 AND g.id=$2 AND NOT u.disabled FOR UPDATE OF g`, s.user, s.connection).Scan(&access, &refresh, &expiry, &hash)
	if err != nil {
		return nil, fmt.Errorf("the Google account that started this scan is disconnected; reconnect and rescan")
	}
	c, err := s.app.runtimeConfig(s.ctx)
	if err != nil || !c.googleEnabled() || hash != googleConfigHash(c) {
		return nil, fmt.Errorf("Google application settings changed; reconnect and rescan")
	}
	at, err := unseal(s.app.C.Secret, access)
	if err != nil {
		return nil, fmt.Errorf("Google credentials unavailable")
	}
	rt, err := unseal(s.app.C.Secret, refresh)
	if err != nil {
		return nil, fmt.Errorf("Google credentials unavailable")
	}
	old := &oauth2.Token{AccessToken: at, RefreshToken: rt, TokenType: "Bearer", Expiry: expiry}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})
	fresh, err := googleOAuthConfig(c).TokenSource(ctx, old).Token()
	if err != nil {
		return nil, fmt.Errorf("Google authorization expired or was revoked; reconnect your account")
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = old.RefreshToken
	}
	// Persist rotated credentials before any dependent Drive request, including
	// when the original request was cancelled after Google issued new credentials.
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer persistCancel()
	_, err = tx.Exec(persistCtx, "UPDATE google_connections SET access_enc=$3,refresh_enc=$4,token_expiry=$5 WHERE user_id=$1 AND id=$2", s.user, s.connection, seal(s.app.C.Secret, fresh.AccessToken), seal(s.app.C.Secret, fresh.RefreshToken), fresh.Expiry)
	if err == nil {
		err = tx.Commit(persistCtx)
	}
	if err != nil {
		return nil, fmt.Errorf("Google refreshed credentials could not be saved")
	}
	return fresh, nil
}
func (a *App) googleReadClient(ctx context.Context, user, connection string) (*http.Client, error) {
	return a.googleReadClientScope(ctx, user, connection, googleDriveScope)
}
func (a *App) googleReadClientScope(ctx context.Context, user, connection, scope string) (*http.Client, error) {
	var client *http.Client
	if user != "" {
		if connection == "" {
			return nil, fmt.Errorf("Google connection unavailable")
		}
		client = oauth2.NewClient(ctx, googleUserTokens{a, ctx, user, connection})
	} else {
		if a.C.GoogleCredentials == "" {
			return nil, fmt.Errorf("connect your Google account or ask an admin to configure Google access")
		}
		data, err := os.ReadFile(a.C.GoogleCredentials)
		if err != nil {
			return nil, fmt.Errorf("cannot read configured Google service-account credentials")
		}
		conf, err := google.JWTConfigFromJSON(data, scope)
		if err != nil {
			return nil, fmt.Errorf("expected Google service-account credentials")
		}
		client = conf.Client(ctx)
	}
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		host := request.URL.Hostname()
		if request.URL.Scheme != "https" || (host != "www.googleapis.com" && host != "sheets.googleapis.com" && host != "content.googleapis.com" && !strings.HasSuffix(host, ".googleusercontent.com")) {
			return fmt.Errorf("Google download redirected outside Google")
		}
		if len(via) >= 5 {
			return fmt.Errorf("too many Google redirects")
		}
		return nil
	}
	return client, nil
}
