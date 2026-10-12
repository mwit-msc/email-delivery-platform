package edp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/crypto/argon2"
	"golang.org/x/oauth2"
	"net"
	"net/http"
	"strings"
	"time"
)

func hashPassword(p string) (string, error) {
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", e
	}
	hash := argon2.IDKey([]byte(p), salt, 3, 64*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}
func passwordParams(encoded string) (salt, hash []byte, m, t uint32, p uint8, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		err = errors.New("invalid password hash")
		return
	}
	_, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p)
	if err != nil || m < 19*1024 || m > 256*1024 || t < 2 || t > 10 || p < 1 || p > 8 {
		err = errors.New("unsupported Argon2 parameters")
		return
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return
	}
	hash, err = base64.RawStdEncoding.DecodeString(parts[5])
	if len(salt) < 16 || len(hash) != 32 {
		err = errors.New("invalid hash length")
	}
	return
}
func validPasswordHash(s string) bool { _, _, _, _, _, e := passwordParams(s); return e == nil }
func checkPassword(encoded, pw string) bool {
	s, h, m, t, p, e := passwordParams(encoded)
	if e != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), s, t, m, p, uint32(len(h)))
	return subtle.ConstantTimeCompare(got, h) == 1
}

var roles = map[string][]string{
	"viewer":         {"app.view", "message.view", "template.view", "campaign.view", "stats.view"},
	"developer":      {"app.view", "message.view", "message.send", "template.view", "template.edit", "key.manage", "campaign.view", "stats.view"},
	"operator":       {"app.view", "message.view", "template.view", "campaign.view", "campaign.edit", "campaign.send", "stats.view"},
	"approver":       {"app.view", "message.view", "template.view", "campaign.view", "campaign.approve", "stats.view"},
	"admin":          {"app.view", "app.manage", "message.view", "message.send", "message.cancel", "template.view", "template.edit", "key.manage", "campaign.view", "campaign.edit", "campaign.approve", "campaign.send", "stats.view", "member.manage"},
	"platform_admin": {"*"},
}

type Principal struct {
	ID, Name, Username, Method, CSRF, Scope, KeyID string
	Root                                           bool
	Groups                                         []string
	GroupsFresh                                    bool
	KeyScopes                                      []string
}

func (a *App) Allowed(ctx context.Context, p *Principal, scope, permission string) bool {
	if p == nil {
		return false
	}
	if p.Root {
		return true
	}
	if p.KeyID != "" {
		return p.Scope == scope && contains(p.KeyScopes, permission)
	}
	var own []string
	rows, e := a.DB.Query(ctx, "SELECT role FROM bindings WHERE user_id=$1 AND scope IN('*',$2)", p.ID, scope)
	if e != nil {
		return false
	}
	for rows.Next() {
		var r string
		rows.Scan(&r)
		own = append(own, r)
	}
	rows.Close()
	if p.Method == "sso" && p.GroupsFresh {
		rows, e = a.DB.Query(ctx, "SELECT role FROM group_mappings WHERE group_slug=ANY($1) AND scope IN('*',$2)", p.Groups, scope)
		if e == nil {
			for rows.Next() {
				var r string
				rows.Scan(&r)
				own = append(own, r)
			}
			rows.Close()
		}
	}
	for _, role := range own {
		if contains(roles[role], "*") || contains(roles[role], permission) {
			return true
		}
	}
	return false
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

type principalKey struct{}

func person(r *http.Request) *Principal {
	p, _ := r.Context().Value(principalKey{}).(*Principal)
	return p
}
func (a *App) session(r *http.Request) *Principal {
	cookie, e := r.Cookie("edp_session")
	if e != nil {
		return nil
	}
	p := &Principal{}
	var expiry *time.Time
	var access, refresh string
	var tokenExpiry *time.Time
	e = a.DB.QueryRow(r.Context(), `SELECT u.id,u.name,u.username,u.root,u.groups,u.groups_expires_at,s.method,s.csrf,s.access_token,s.refresh_token,s.token_expiry FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND NOT u.disabled`, tokenHash(cookie.Value)).Scan(&p.ID, &p.Name, &p.Username, &p.Root, &p.Groups, &expiry, &p.Method, &p.CSRF, &access, &refresh, &tokenExpiry)
	if e != nil {
		return nil
	}
	p.GroupsFresh = expiry != nil && expiry.After(time.Now())
	if p.Method == "sso" && !p.GroupsFresh {
		if c, err := a.runtimeConfig(r.Context()); err == nil && c.ssoEnabled() {
			_ = a.refreshIdentity(r.Context(), p, tokenHash(cookie.Value))
		}
	}

	return p
}

// Serialize refresh rotation per session, and persist the successor before UserInfo.
// Group grants remain stale unless the independently fetched claims are validated.
func (a *App) refreshIdentity(ctx context.Context, p *Principal, sessionHash string) error {
	networkCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, e := a.runtimeConfig(networkCtx)
	if e != nil {
		return e
	}
	provider, conf, e := oidcConfigFor(networkCtx, c)
	if e != nil {
		return e
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var access, refresh, issuer, subject string
	var tokenExpiry, groupExpiry *time.Time
	var groups []string
	e = tx.QueryRow(ctx, `SELECT s.access_token,s.refresh_token,s.token_expiry,u.oidc_issuer,u.oidc_subject,u.groups,u.groups_expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND NOT u.disabled FOR UPDATE OF s`, sessionHash).Scan(&access, &refresh, &tokenExpiry, &issuer, &subject, &groups, &groupExpiry)
	if e != nil {
		return e
	}
	if issuer != c.Issuer {
		return fmt.Errorf("session identity provider changed")
	}
	if groupExpiry != nil && groupExpiry.After(time.Now()) {
		p.Groups = groups
		p.GroupsFresh = true
		return tx.Commit(ctx)
	}
	at, e := unseal(a.C.Secret, access)
	if e != nil {
		return e
	}
	rt, e := unseal(a.C.Secret, refresh)
	if e != nil {
		return e
	}
	tok := &oauth2.Token{AccessToken: at, RefreshToken: rt, TokenType: "Bearer"}
	if tokenExpiry != nil {
		tok.Expiry = *tokenExpiry
	}
	fresh, e := conf.TokenSource(networkCtx, tok).Token()
	if e != nil {
		return e
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = rt
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer persistCancel()
	_, e = tx.Exec(persistCtx, "UPDATE sessions SET access_token=$2,refresh_token=$3,token_expiry=$4 WHERE token_hash=$1", sessionHash, seal(a.C.Secret, fresh.AccessToken), seal(a.C.Secret, fresh.RefreshToken), fresh.Expiry)
	if e != nil {
		return e
	}
	if e = tx.Commit(persistCtx); e != nil {
		return e
	}
	info, e := provider.UserInfo(networkCtx, oauth2.StaticTokenSource(fresh))
	if e != nil {
		return e
	}
	if info.Subject != subject {
		return fmt.Errorf("UserInfo subject mismatch")
	}
	_, groups, e = oidcUserClaims(info, c.GroupsClaim)
	if e != nil {
		return e
	}
	_, e = a.DB.Exec(ctx, "UPDATE users SET groups=$2,groups_expires_at=now()+interval '5 minutes' WHERE id=$1 AND NOT disabled", p.ID, groups)
	if e != nil {
		return e
	}
	p.Groups = groups
	p.GroupsFresh = true
	return nil
}

func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := a.session(r)
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			p = a.keyPrincipal(r)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
func (a *App) keyPrincipal(r *http.Request) *Principal {
	v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if v == r.Header.Get("Authorization") || v == "" {
		return nil
	}
	p := &Principal{Method: "key"}
	e := a.DB.QueryRow(r.Context(), "SELECT id,application_id,scopes FROM api_keys WHERE token_hash=$1 AND NOT revoked AND (expires_at IS NULL OR expires_at>now())", tokenHash(v)).Scan(&p.KeyID, &p.Scope, &p.KeyScopes)
	if e != nil {
		return nil
	}
	p.ID = p.KeyID
	return p
}
func (a *App) require(w http.ResponseWriter, r *http.Request, scope, permission string) bool {
	p := person(r)
	if p == nil {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			problem(w, 401, "authentication required")
		} else if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusNoContent)
		} else {
			http.Redirect(w, r, "/login", 303)
		}
		return false
	}
	if !a.Allowed(r.Context(), p, scope, permission) {
		problem(w, 403, "permission denied")
		return false
	}
	if r.Method != "GET" && r.Method != "HEAD" && p.KeyID == "" {
		var formErr error
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			formErr = r.ParseMultipartForm(1 << 20)
		} else {
			formErr = r.ParseForm()
		}
		if formErr != nil {
			problem(w, 400, "invalid or oversized form")
			return false
		}
		if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(p.CSRF)) != 1 && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(p.CSRF)) != 1 {
			problem(w, 403, "invalid CSRF token")
			return false
		}
	}
	return true
}
func (a *App) newSession(w http.ResponseWriter, r *http.Request, id, method, access, refresh string, expiry time.Time) error {
	token := newID("ses_")
	_, e := a.DB.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,method,csrf,access_token,refresh_token,token_expiry,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", tokenHash(token), id, method, newID("csrf_"), seal(a.C.Secret, access), seal(a.C.Secret, refresh), expiry, time.Now().Add(sessionLife))
	if e != nil {
		return e
	}
	http.SetCookie(w, &http.Cookie{Name: "edp_session", Value: token, Path: "/", HttpOnly: true, Secure: !a.C.Dev, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionLife.Seconds())})
	a.audit(r.Context(), id, "login."+method, "", "")
	return nil
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		c, _ := a.runtimeConfig(r.Context())
		a.render(w, r, "login", map[string]any{"SSO": c.ssoEnabled(), "Error": r.URL.Query().Get("error")})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	r.ParseForm()
	cookie, e := r.Cookie("edp_login_csrf")
	if e != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.FormValue("csrf"))) != 1 {
		problem(w, 403, "invalid login CSRF token")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	ip, _, ipErr := net.SplitHostPort(r.RemoteAddr)
	if ipErr != nil {
		ip = r.RemoteAddr
	}
	for _, b := range []string{"login:ip:" + ip, "login:user:" + strings.ToLower(username)} {
		var count int
		e = a.DB.QueryRow(r.Context(), `INSERT INTO login_attempts(bucket,attempts,expires_at) VALUES($1,1,now()+interval '15 minutes') ON CONFLICT(bucket) DO UPDATE SET attempts=CASE WHEN login_attempts.expires_at<now() THEN 1 ELSE login_attempts.attempts+1 END,expires_at=CASE WHEN login_attempts.expires_at<now() THEN now()+interval '15 minutes' ELSE login_attempts.expires_at END RETURNING attempts`, b).Scan(&count)
		if e != nil || count > 15 {
			w.Header().Set("Retry-After", "900")
			problem(w, 429, "too many login attempts; retry later")
			return
		}
	}
	var id, hash string
	e = a.DB.QueryRow(r.Context(), "SELECT id,password_hash FROM users WHERE username=$1 AND NOT disabled", username).Scan(&id, &hash)
	ok := checkPassword(hash, r.FormValue("password"))
	if e != nil || !ok {
		a.audit(r.Context(), "anonymous", "login.failed", "", "")
		c, _ := a.runtimeConfig(r.Context())
		a.render(w, r, "login", map[string]any{"SSO": c.ssoEnabled(), "Error": "Invalid username or password"})
		return
	}
	if e = a.newSession(w, r, id, "local", "", "", time.Time{}); e != nil {
		problem(w, 500, "could not create session")
		return
	}
	http.Redirect(w, r, "/", 303)
}
func (a *App) oidcConfig(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	c, e := a.runtimeConfig(ctx)
	if e != nil {
		return nil, nil, e
	}
	return oidcConfigFor(ctx, c)
}
func oidcConfigFor(ctx context.Context, c Config) (*oidc.Provider, *oauth2.Config, error) {
	if !c.ssoEnabled() || c.Issuer == "" {
		return nil, nil, fmt.Errorf("SSO is not configured")
	}
	provider, e := oidc.NewProvider(ctx, c.Issuer)
	if e != nil {
		return nil, nil, e
	}
	scopes := c.OIDCScopes
	if scopes == nil {
		scopes = []string{"openid", "profile", "email", "groups", "offline_access"}
	}
	return provider, &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: c.PublicURL + "/auth/callback", Scopes: scopes}, nil
}
func oidcUserClaims(info *oidc.UserInfo, groupsClaim string) (string, []string, error) {
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	var claims map[string]json.RawMessage
	if e := info.Claims(&claims); e != nil {
		return "", nil, e
	}
	var name string
	if raw, ok := claims["name"]; ok {
		if e := json.Unmarshal(raw, &name); e != nil {
			return "", nil, e
		}
	}
	groups := []string{}
	if raw, ok := claims[groupsClaim]; ok {
		if e := json.Unmarshal(raw, &groups); e != nil {
			return "", nil, fmt.Errorf("invalid groups claim: %w", e)
		}
		if groups == nil {
			groups = []string{}
		}
	}
	return name, groups, nil
}
func (a *App) ssoStart(w http.ResponseWriter, r *http.Request) {
	c, e := a.runtimeConfig(r.Context())
	if e != nil {
		problem(w, 503, "SSO configuration unavailable; use local login")
		return
	}
	if !c.ssoEnabled() {
		problem(w, 503, "SSO is not configured; use local login")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, conf, e := oidcConfigFor(ctx, c)
	if e != nil {
		http.Redirect(w, r, "/login?error=SSO+unavailable.+Use+local+login.", 303)
		return
	}
	state, nonce, verifier := newID("state_"), newID("nonce_"), oauth2.GenerateVerifier()
	_, e = a.DB.Exec(ctx, "INSERT INTO oauth_states(state_hash,verifier,nonce,expires_at) VALUES($1,$2,$3,now()+interval '10 minutes')", tokenHash(state), verifier, nonce)
	if e != nil {
		problem(w, 500, "could not start login")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "edp_oidc_state", Value: state, Path: "/auth", HttpOnly: true, Secure: !a.C.Dev, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), 302)
}
func (a *App) ssoCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	cookie, e := r.Cookie("edp_oidc_state")
	if e != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		problem(w, 400, "invalid login state")
		return
	}
	var verifier, nonce string
	e = a.DB.QueryRow(r.Context(), "DELETE FROM oauth_states WHERE state_hash=$1 AND expires_at>now() RETURNING verifier,nonce", tokenHash(state)).Scan(&verifier, &nonce)
	if e != nil {
		problem(w, 400, "login expired; try again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	c, e := a.runtimeConfig(ctx)
	if e != nil {
		problem(w, 503, "SSO configuration unavailable")
		return
	}
	provider, conf, e := oidcConfigFor(ctx, c)
	if e != nil {
		problem(w, 503, "SSO unavailable")
		return
	}
	tok, e := conf.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if e != nil {
		problem(w, 400, "code exchange failed")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idToken, e := provider.Verifier(&oidc.Config{ClientID: conf.ClientID}).Verify(ctx, raw)
	if e != nil || idToken.Nonce != nonce {
		problem(w, 400, "invalid identity token")
		return
	}
	info, e := provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if e != nil || info.Subject != idToken.Subject {
		problem(w, 400, "invalid user information")
		return
	}
	name, groups, e := oidcUserClaims(info, c.GroupsClaim)
	if e != nil {
		problem(w, 400, "invalid claims")
		return
	}
	if name == "" {
		name = "SSO user"
	}
	var id string
	e = a.DB.QueryRow(ctx, `INSERT INTO users(id,username,name,oidc_issuer,oidc_subject,groups,groups_expires_at) VALUES($1,$2,$3,$4,$5,$6,now()+interval '5 minutes') ON CONFLICT(oidc_issuer,oidc_subject) DO UPDATE SET name=EXCLUDED.name,groups=EXCLUDED.groups,groups_expires_at=EXCLUDED.groups_expires_at RETURNING id`, newID("usr_"), "sso_"+tokenHash(c.Issuer + "|" + idToken.Subject)[:24], name, c.Issuer, idToken.Subject, groups).Scan(&id)
	if e != nil {
		problem(w, 500, "could not save identity")
		return
	}
	var disabled bool
	a.DB.QueryRow(ctx, "SELECT disabled FROM users WHERE id=$1", id).Scan(&disabled)
	if disabled {
		problem(w, 403, "account disabled")
		return
	}
	if e = a.newSession(w, r, id, "sso", tok.AccessToken, tok.RefreshToken, tok.Expiry); e != nil {
		problem(w, 500, "could not create session")
		return
	}
	http.Redirect(w, r, "/", 303)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	p := person(r)
	if p == nil {
		http.Redirect(w, r, "/login", 303)
		return
	}
	r.ParseForm()
	if r.FormValue("csrf") != p.CSRF {
		problem(w, 403, "invalid CSRF token")
		return
	}
	if c, e := r.Cookie("edp_session"); e == nil {
		a.DB.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "edp_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: !a.C.Dev, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", 303)
}
