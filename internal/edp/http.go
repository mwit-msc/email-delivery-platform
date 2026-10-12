package edp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func problem(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": message})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func (a *App) query(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	rows, e := a.DB.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		vals, e := rows.Values()
		if e != nil {
			return nil, e
		}
		row := map[string]any{}
		for i, col := range rows.FieldDescriptions() {
			row[col.Name] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
func (a *App) one(ctx context.Context, sql string, args ...any) (map[string]any, error) {
	rows, e := a.query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, pgx.ErrNoRows
	}
	return rows[0], nil
}
func (a *App) visibleApps(r *http.Request) []map[string]any {
	rows, e := a.query(r.Context(), "SELECT * FROM applications ORDER BY name")
	if e != nil {
		return nil
	}
	out := []map[string]any{}
	for _, row := range rows {
		if a.Allowed(r.Context(), person(r), row["id"].(string), "app.view") {
			out = append(out, row)
		}
	}
	return out
}
func (a *App) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	p := person(r)
	data["User"] = p
	data["Version"] = a.Version
	data["Page"] = page
	data["Apps"] = a.visibleApps(r)
	data["Platform"] = a.Allowed(r.Context(), p, "*", "platform.manage")
	data["Dev"] = a.C.Dev
	config, configErr := a.runtimeConfig(r.Context())
	if configErr != nil {
		problem(w, 500, "runtime configuration unavailable")
		return
	}
	data["ProviderName"] = config.ProviderName
	if data["ProviderName"] == "" {
		data["ProviderName"] = "Single sign-on"
	}
	data["Can"] = func(scope, permission string) bool { return a.Allowed(r.Context(), p, scope, permission) }
	if data["Scope"] == nil {
		data["Scope"] = r.URL.Query().Get("app")
	}
	if p != nil {
		data["CSRF"] = p.CSRF
	} else {
		c, e := r.Cookie("edp_login_csrf")
		if e != nil || len(c.Value) < 32 {
			value := newID("csrf_")
			http.SetCookie(w, &http.Cookie{Name: "edp_login_csrf", Value: value, Path: "/login", HttpOnly: true, Secure: !a.C.Dev, SameSite: http.SameSiteStrictMode, MaxAge: 600})
			data["CSRF"] = value
		} else {
			data["CSRF"] = c.Value
		}
	}
	funcs := template.FuncMap{"initial": func(s string) string {
		r := []rune(strings.TrimSpace(s))
		if len(r) == 0 {
			return "?"
		}
		return strings.ToUpper(string(r[0]))
	}, "timefmt": func(v any) string {
		if t, ok := v.(time.Time); ok {
			return t.In(time.FixedZone("Bangkok", 7*3600)).Format("02 Jan 15:04")
		}
		return fmt.Sprint(v)
	}, "json": func(v any) string { b, _ := json.Marshal(v); return string(b) }, "join": func(v any) string {
		switch x := v.(type) {
		case []string:
			return strings.Join(x, ", ")
		case []any:
			var s []string
			for _, a := range x {
				s = append(s, fmt.Sprint(a))
			}
			return strings.Join(s, ", ")
		}
		return ""
	}, "inc": func(n int) int { return n + 1 }}
	views, e := template.New("views").Funcs(funcs).ParseFS(resources, "web/views/*.html")
	if e != nil {
		slog.Error("templates", "error", e)
		problem(w, 500, "could not render page")
		return
	}
	var content bytes.Buffer
	if e = views.ExecuteTemplate(&content, page, data); e != nil {
		slog.Error("render", "page", page, "error", e)
		problem(w, 500, "could not render page")
		return
	}
	data["Body"] = template.HTML(content.String())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if e = views.ExecuteTemplate(w, "layout", data); e != nil {
		slog.Error("layout", "error", e)
	}
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(resources, "web/assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /demo/logo.png", func(w http.ResponseWriter, r *http.Request) {
		if a.C.SeedLogoFile == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, a.C.SeedLogoFile)
	})
	mux.HandleFunc("GET /assets/logo.png", func(w http.ResponseWriter, r *http.Request) {
		if a.C.SeedLogoFile == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, a.C.SeedLogoFile)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if a.DB.Ping(ctx) != nil {
			problem(w, 503, "database unavailable")
			return
		}
		respond(w, 200, map[string]any{"status": "ok", "version": a.Version})
	})
	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("GET /auth/sso", a.ssoStart)
	mux.HandleFunc("GET /auth/callback", a.ssoCallback)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /statistics", a.statistics)
	mux.HandleFunc("GET /search", a.search)
	mux.HandleFunc("GET /metrics", a.metrics)
	mux.HandleFunc("GET /applications", a.applications)
	mux.HandleFunc("POST /applications", a.applications)
	mux.HandleFunc("GET /applications/{id}", a.application)
	mux.HandleFunc("POST /applications/{id}", a.application)
	mux.HandleFunc("GET /templates", a.templates)
	mux.HandleFunc("POST /templates", a.templates)
	mux.HandleFunc("GET /templates/{id}", a.templateEditor)
	mux.HandleFunc("POST /templates/{id}", a.templateEditor)
	mux.HandleFunc("GET /templates/{id}/data", a.editorData)
	mux.HandleFunc("POST /templates/{id}/data", a.editorData)
	mux.HandleFunc("GET /campaigns", a.campaigns)
	mux.HandleFunc("POST /campaigns", a.campaigns)
	mux.HandleFunc("GET /campaigns/{id}/progress", a.campaignProgress)
	mux.HandleFunc("GET /campaigns/{id}", a.campaign)
	mux.HandleFunc("POST /campaigns/{id}", a.campaign)
	mux.HandleFunc("GET /campaign-rows/{row}/preview", a.campaignPreview)
	mux.HandleFunc("GET /messages", a.messages)
	mux.HandleFunc("GET /messages/{id}", a.message)
	mux.HandleFunc("POST /messages/{id}", a.message)
	mux.HandleFunc("GET /messages/{id}/preview", a.messagePreview)
	mux.HandleFunc("GET /access", a.access)
	mux.HandleFunc("POST /access", a.access)
	mux.HandleFunc("GET /settings", a.settings)
	mux.HandleFunc("POST /settings", a.settings)
	mux.HandleFunc("GET /audit", a.auditPage)
	mux.HandleFunc("POST /v1/messages", a.apiSubmit)
	mux.HandleFunc("GET /v1/messages/{id}", a.apiMessage)
	mux.HandleFunc("POST /v1/messages/{id}/cancel", a.apiCancel)
	mux.HandleFunc("POST /v1/delivery-events", a.deliveryFeedback)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		defer func() {
			if v := recover(); v != nil {
				slog.Error("handler panic", "error", v)
				problem(w, 500, "internal error")
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data:; font-src 'self' data:; frame-src 'self' blob:; connect-src 'self'; base-uri 'self'; form-action 'self'")
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		a.authenticate(mux).ServeHTTP(w, r)
	})
}
func (a *App) Serve(ctx context.Context) error {
	if e := a.Seed(ctx, false); e != nil {
		return e
	}
	server := &http.Server{Addr: a.C.Address, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	slog.Info("HTTP server started", "address", a.C.Address, "version", a.Version)
	e := server.ListenAndServe()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
func (a *App) applications(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		if !a.require(w, r, "*", "platform.manage") {
			return
		}
		name, from := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("from"))
		if name == "" || !validAddress(from) {
			problem(w, 400, "name and valid sender address required")
			return
		}
		id := newID("app_")
		_, e := a.DB.Exec(r.Context(), "INSERT INTO applications(id,name,from_address) VALUES($1,$2,$3)", id, name, from)
		if e != nil {
			problem(w, 500, "could not create application")
			return
		}
		a.audit(r.Context(), person(r).ID, "application.create", id, name)
		http.Redirect(w, r, "/applications/"+id, 303)
		return
	}
	if person(r) == nil {
		http.Redirect(w, r, "/login", 303)
		return
	}
	a.render(w, r, "applications", map[string]any{"Rows": a.visibleApps(r)})
}
func (a *App) application(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	permission := "app.view"
	if r.Method == "POST" {
		permission = "app.manage"
		if r.FormValue("action") == "key" || r.FormValue("action") == "revoke" {
			permission = "key.manage"
		}
	}
	if !a.require(w, r, id, permission) {
		return
	}
	data, e := a.one(r.Context(), "SELECT * FROM applications WHERE id=$1", id)
	if e != nil {
		problem(w, 404, "application not found")
		return
	}
	var secret string
	if r.Method == "POST" {
		switch r.FormValue("action") {
		case "key":
			if !a.Allowed(r.Context(), person(r), id, "key.manage") {
				problem(w, 403, "permission denied")
				return
			}
			scopes := r.Form["scopes"]
			valid := []string{"message.send", "message.otp", "message.view", "message.cancel", "delivery.report"}
			for _, s := range scopes {
				if s == "delivery.report" && !a.Allowed(r.Context(), person(r), "*", "platform.manage") {
					problem(w, 403, "only platform administrators may provision delivery-report credentials")
					return
				}
				if !contains(valid, s) {
					problem(w, 400, "invalid key scope")
					return
				}
			}
			secret = newID("edp_")
			expires := time.Now().Add(90 * 24 * time.Hour)
			_, e = a.DB.Exec(r.Context(), "INSERT INTO api_keys(id,application_id,name,token_hash,scopes,expires_at) VALUES($1,$2,$3,$4,$5,$6)", newID("key_"), id, r.FormValue("name"), tokenHash(secret), scopes, expires)
		case "revoke":
			if !a.Allowed(r.Context(), person(r), id, "key.manage") {
				problem(w, 403, "permission denied")
				return
			}
			_, e = a.DB.Exec(r.Context(), "UPDATE api_keys SET revoked=true WHERE id=$1 AND application_id=$2", r.FormValue("key"), id)
		default:
			from, reply := strings.TrimSpace(r.FormValue("from")), strings.TrimSpace(r.FormValue("reply"))
			if !validAddress(from) || reply != "" && !validAddress(reply) {
				problem(w, 400, "invalid sender or reply address")
				return
			}
			n := func(k string) int { v, _ := strconv.Atoi(r.FormValue(k)); return v }
			daily, rate, recipient, queue := n("daily"), n("rate"), n("recipient"), n("queue")
			if !a.Allowed(r.Context(), person(r), "*", "platform.manage") {
				same := func(key string, value int) bool { return fmt.Sprint(data[key]) == strconv.Itoa(value) }
				if from != data["from_address"] || !same("daily_limit", daily) || !same("rate_per_minute", rate) || !same("recipient_hour_limit", recipient) || !same("queue_limit", queue) {
					problem(w, 403, "sender identities and quotas are managed by platform administrators")
					return
				}
			}
			if daily < 1 || rate < 1 || recipient < 1 || queue < 1 {
				problem(w, 400, "limits must be positive")
				return
			}
			fallback, _ := data["fallback_enabled"].(bool)
			if a.Allowed(r.Context(), person(r), "*", "platform.manage") {
				fallback = r.FormValue("fallback_enabled") == "on"
			}
			_, e = a.DB.Exec(r.Context(), "UPDATE applications SET name=$2,from_address=$3,reply_to=$4,daily_limit=$5,rate_per_minute=$6,recipient_hour_limit=$7,queue_limit=$8,paused=$9,fallback_enabled=$10 WHERE id=$1", id, r.FormValue("name"), from, reply, daily, rate, recipient, queue, r.FormValue("paused") == "on", fallback)
		}
		if e != nil {
			problem(w, 400, "could not update application")
			return
		}
		a.audit(r.Context(), person(r).ID, "application."+r.FormValue("action"), id, "")
		data, _ = a.one(r.Context(), "SELECT * FROM applications WHERE id=$1", id)
	}
	var keys []map[string]any
	if a.Allowed(r.Context(), person(r), id, "key.manage") {
		keys, _ = a.query(r.Context(), "SELECT id,name,scopes,revoked,created_at,expires_at FROM api_keys WHERE application_id=$1 ORDER BY created_at DESC", id)
	}
	config, err := a.runtimeConfig(r.Context())
	if err != nil {
		problem(w, 500, "runtime configuration unavailable")
		return
	}
	a.render(w, r, "application", map[string]any{"Application": data, "Keys": keys, "Secret": secret, "Scope": id, "SecondaryConfigured": config.SecondarySMTPAddress != ""})
}
func (a *App) apiSubmit(w http.ResponseWriter, r *http.Request) {
	p := person(r)
	if p == nil {
		problem(w, 401, "service API key required")
		return
	}
	if !a.require(w, r, p.Scope, "message.send") {
		return
	}
	var in SendInput
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		problem(w, 400, "invalid JSON payload")
		return
	}
	if in.ApplicationID != "" && in.ApplicationID != p.Scope {
		problem(w, 403, "key belongs to another application")
		return
	}
	in.ApplicationID = p.Scope
	if in.Class == "otp" && !a.Allowed(r.Context(), p, p.Scope, "message.otp") {
		problem(w, 403, "key does not allow OTP priority")
		return
	}
	id, replayed, e := a.Submit(r.Context(), in, r.Header.Get("Idempotency-Key"))
	if e != nil {
		var h *HTTPError
		if errors.As(e, &h) {
			if h.Code == 429 {
				w.Header().Set("Retry-After", "60")
			}
			problem(w, h.Code, h.Message)
		} else {
			slog.Error("submission", "error", e)
			problem(w, 500, "message could not be persisted")
		}
		return
	}
	respond(w, 202, map[string]any{"id": id, "replayed": replayed, "status_url": "/v1/messages/" + id})
}
func (a *App) apiMessage(w http.ResponseWriter, r *http.Request) {
	data, e := a.one(r.Context(), "SELECT id,application_id,status,attempts,last_error,route,gateway_status,failure_category,created_at,relay_accepted_at,expires_at FROM messages WHERE id=$1", r.PathValue("id"))
	if e != nil {
		problem(w, 404, "message not found")
		return
	}
	if !a.require(w, r, data["application_id"].(string), "message.view") {
		return
	}
	respond(w, 200, data)
}
func (a *App) cancelMessage(ctx context.Context, id string) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, "UPDATE messages SET status='cancelled',updated_at=now() WHERE id=$1 AND status IN('queued','retry_wait')", id)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("message has already crossed the dispatch boundary")
	}
	_, e = tx.Exec(ctx, "INSERT INTO delivery_events(message_id,status) VALUES($1,'cancelled')", id)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) apiCancel(w http.ResponseWriter, r *http.Request) {
	var scope string
	e := a.DB.QueryRow(r.Context(), "SELECT application_id FROM messages WHERE id=$1", r.PathValue("id")).Scan(&scope)
	if e != nil {
		problem(w, 404, "message not found")
		return
	}
	if !a.require(w, r, scope, "message.cancel") {
		return
	}
	if e = a.cancelMessage(r.Context(), r.PathValue("id")); e != nil {
		problem(w, 409, e.Error())
		return
	}
	respond(w, 200, map[string]string{"status": "cancelled"})
}
func (a *App) deliveryFeedback(w http.ResponseWriter, r *http.Request) {
	p := person(r)
	if p == nil {
		problem(w, 401, "service API key required")
		return
	}
	if !a.require(w, r, p.Scope, "delivery.report") {
		return
	}
	var in struct {
		MessageID string `json:"message_id"`
		Status    string `json:"status"`
		EventKey  string `json:"event_key"`
		Detail    string `json:"detail"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.EventKey == "" || len(in.EventKey) > 200 || len(in.Detail) > 512 || !contains([]string{"remote_accepted", "bounced"}, in.Status) {
		problem(w, 400, "provide message_id, event_key, and status remote_accepted or bounced")
		return
	}
	tx, e := a.DB.Begin(r.Context())
	if e != nil {
		problem(w, 500, "could not persist delivery event")
		return
	}
	defer tx.Rollback(r.Context())
	var state string
	e = tx.QueryRow(r.Context(), "SELECT status FROM messages WHERE id=$1 AND application_id=$2 FOR UPDATE", in.MessageID, p.Scope).Scan(&state)
	if e != nil {
		problem(w, 404, "message not found")
		return
	}
	key := p.Scope + ":" + in.EventKey
	var oldID, oldStatus string
	e = tx.QueryRow(r.Context(), "SELECT message_id,status FROM delivery_events WHERE event_key=$1", key).Scan(&oldID, &oldStatus)
	if e == nil {
		if oldID != in.MessageID || oldStatus != in.Status {
			problem(w, 409, "event key already used")
			return
		}
		respond(w, 200, map[string]any{"replayed": true})
		return
	}
	if e != pgx.ErrNoRows {
		problem(w, 500, "could not read delivery event")
		return
	}
	if !contains([]string{"relay_accepted", "unknown", "remote_accepted"}, state) {
		problem(w, 409, "message cannot accept this delivery event in its current state")
		return
	}
	_, e = tx.Exec(r.Context(), "UPDATE messages SET status=$2,updated_at=now(),last_error=CASE WHEN $2='bounced' THEN $3 ELSE '' END WHERE id=$1", in.MessageID, in.Status, in.Detail)
	if e == nil {
		_, e = tx.Exec(r.Context(), "INSERT INTO delivery_events(message_id,status,detail,event_key) VALUES($1,$2,$3,$4)", in.MessageID, in.Status, in.Detail, key)
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		problem(w, 500, "could not persist delivery event")
		return
	}
	respond(w, 200, map[string]string{"status": in.Status})
}
func (a *App) messages(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("app")
	if !a.require(w, r, scope, "message.view") {
		return
	}
	status := r.URL.Query().Get("status")
	rows, e := a.query(r.Context(), "SELECT id,recipient,subject,class,status,attempts,last_error,created_at FROM messages WHERE application_id=$1 AND ($2='' OR status=$2) ORDER BY created_at DESC LIMIT 200", scope, status)
	if e != nil {
		problem(w, 500, "could not load messages")
		return
	}
	a.render(w, r, "messages", map[string]any{"Rows": rows, "Scope": scope, "Status": status})
}
func (a *App) message(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, e := a.one(r.Context(), "SELECT * FROM messages WHERE id=$1", id)
	if e != nil {
		problem(w, 404, "message not found")
		return
	}
	scope := data["application_id"].(string)
	perm := "message.view"
	if r.Method == "POST" {
		perm = "message.cancel"
	}
	if !a.require(w, r, scope, perm) {
		return
	}
	if r.Method == "POST" {
		if e = a.cancelMessage(r.Context(), id); e != nil {
			problem(w, 409, e.Error())
			return
		}
		a.audit(r.Context(), person(r).ID, "message.cancel", scope, id)
		http.Redirect(w, r, "/messages/"+id, 303)
		return
	}
	events, _ := a.query(r.Context(), "SELECT status,detail,created_at FROM delivery_events WHERE message_id=$1 ORDER BY created_at", id)
	a.render(w, r, "message", map[string]any{"Message": data, "Events": events, "Scope": scope})
}
func (a *App) messagePreview(w http.ResponseWriter, r *http.Request) {
	var scope, content string
	e := a.DB.QueryRow(r.Context(), "SELECT application_id,html FROM messages WHERE id=$1", r.PathValue("id")).Scan(&scope, &content)
	if e != nil {
		problem(w, 404, "message not found")
		return
	}
	if !a.require(w, r, scope, "message.view") {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src https: http: data:; style-src 'unsafe-inline'")
	w.Write([]byte(content))
}
