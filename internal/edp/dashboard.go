package edp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (a *App) stats(ctx context.Context, scope string) (map[string]any, error) {
	totals, e := a.one(ctx, `SELECT count(*) AS total,count(*) FILTER(WHERE status IN('queued','retry_wait','sending')) AS queued,count(*) FILTER(WHERE status IN('relay_accepted','remote_accepted')) AS accepted,count(*) FILTER(WHERE status='bounced') AS bounced,count(*) FILTER(WHERE status='failed') AS failed,count(*) FILTER(WHERE status='unknown') AS unknown,coalesce(extract(epoch FROM now()-min(created_at) FILTER(WHERE status IN('queued','retry_wait'))),0)::float8 AS oldest_seconds,coalesce(percentile_cont(0.95) WITHIN GROUP(ORDER BY extract(epoch FROM relay_accepted_at-created_at)) FILTER(WHERE relay_accepted_at IS NOT NULL),0)::float8 AS p95_seconds FROM messages WHERE ($1='' OR application_id=$1) AND created_at>now()-interval '7 days'`, scope)
	if e != nil {
		return nil, e
	}
	series, e := a.query(ctx, `WITH buckets AS(SELECT generate_series(date_trunc('day',now()-interval '6 days'),date_trunc('day',now()),interval '1 day') AS day),submitted AS(SELECT date_trunc('day',created_at) AS day,count(*) AS n FROM messages WHERE ($1='' OR application_id=$1) AND created_at>now()-interval '7 days' GROUP BY 1),accepted AS(SELECT date_trunc('day',e.created_at) AS day,count(DISTINCT e.message_id) AS n FROM delivery_events e JOIN messages m ON m.id=e.message_id WHERE e.status='relay_accepted' AND ($1='' OR m.application_id=$1) AND e.created_at>now()-interval '7 days' GROUP BY 1),failures AS(SELECT date_trunc('day',e.created_at) AS day,count(DISTINCT e.message_id) AS n FROM delivery_events e JOIN messages m ON m.id=e.message_id WHERE e.status IN('failed','bounced') AND ($1='' OR m.application_id=$1) AND e.created_at>now()-interval '7 days' GROUP BY 1) SELECT to_char(b.day AT TIME ZONE 'Asia/Bangkok','DD Mon') AS label,coalesce(s.n,0) AS submitted,coalesce(a.n,0) AS accepted,coalesce(f.n,0) AS failed FROM buckets b LEFT JOIN submitted s USING(day) LEFT JOIN accepted a USING(day) LEFT JOIN failures f USING(day) ORDER BY b.day`, scope)
	if e != nil {
		return nil, e
	}
	byApp, e := a.query(ctx, `SELECT a.name,count(m.id) AS total FROM applications a LEFT JOIN messages m ON m.application_id=a.id AND m.created_at>now()-interval '7 days' WHERE ($1='' OR a.id=$1) GROUP BY a.id ORDER BY total DESC`, scope)
	if e != nil {
		return nil, e
	}
	return map[string]any{"totals": totals, "series": series, "applications": byApp}, nil
}
func (a *App) statsScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	scope := r.URL.Query().Get("app")
	if person(r) == nil {
		http.Redirect(w, r, "/login", 303)
		return "", false
	}
	if scope == "" && !a.Allowed(r.Context(), person(r), "*", "platform.manage") {
		apps := a.visibleApps(r)
		if len(apps) == 0 {
			return "", true
		}
		scope = apps[0]["id"].(string)
	}
	if scope != "" && !a.require(w, r, scope, "stats.view") {
		return "", false
	}
	return scope, true
}
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.statsScope(w, r)
	if !ok {
		return
	}
	if scope == "" && !a.Allowed(r.Context(), person(r), "*", "platform.manage") {
		a.render(w, r, "dashboard", map[string]any{"NoAccess": true})
		return
	}
	stats, e := a.stats(r.Context(), scope)
	if e != nil {
		problem(w, 500, "could not load statistics")
		return
	}
	settings, _ := a.one(r.Context(), "SELECT * FROM platform_settings WHERE id=1")
	recent, _ := a.query(r.Context(), "SELECT m.id,m.recipient,m.subject,m.status,m.created_at,a.name AS application FROM messages m JOIN applications a ON a.id=m.application_id WHERE ($1='' OR m.application_id=$1) ORDER BY m.created_at DESC LIMIT 12", scope)
	a.render(w, r, "dashboard", map[string]any{"Stats": stats, "Settings": settings, "Recent": recent, "Scope": scope})
}
func (a *App) statistics(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.statsScope(w, r)
	if !ok {
		return
	}
	if scope == "" && !a.Allowed(r.Context(), person(r), "*", "platform.manage") {
		problem(w, 403, "no application access")
		return
	}
	data, e := a.stats(r.Context(), scope)
	if e != nil {
		problem(w, 500, "statistics unavailable")
		return
	}
	respond(w, 200, data)
}
func (a *App) metrics(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	monitoring := a.C.MetricsToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.C.MetricsToken)) == 1
	if !monitoring && !a.require(w, r, "*", "platform.manage") {
		return
	}
	rows, e := a.query(r.Context(), "SELECT status,count(*) AS count FROM messages GROUP BY status")
	if e != nil {
		problem(w, 503, "metrics unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP edp_messages Number of messages by current delivery state.")
	fmt.Fprintln(w, "# TYPE edp_messages gauge")
	for _, row := range rows {
		fmt.Fprintf(w, "edp_messages{status=%q} %v\n", row["status"], row["count"])
	}
	var oldest float64
	a.DB.QueryRow(r.Context(), "SELECT coalesce(extract(epoch FROM now()-min(created_at)),0)::float8 FROM messages WHERE status IN('queued','retry_wait')").Scan(&oldest)
	fmt.Fprintf(w, "# TYPE edp_queue_oldest_seconds gauge\nedp_queue_oldest_seconds %f\n", oldest)
}
func (a *App) templates(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("app")
	if r.Method == "POST" {
		r.ParseForm()
		scope = r.FormValue("app")
		if !a.require(w, r, scope, "template.edit") {
			return
		}
		slug := strings.TrimSpace(r.FormValue("slug"))
		if !regexpSlug(slug) {
			problem(w, 400, "template ID may contain lowercase letters, numbers, dots, hyphens, and underscores")
			return
		}
		id := newID("tpl_")
		tx, e := a.DB.Begin(r.Context())
		if e != nil {
			problem(w, 500, "could not create template")
			return
		}
		defer tx.Rollback(r.Context())
		_, e = tx.Exec(r.Context(), "INSERT INTO templates(id,application_id,slug,name) VALUES($1,$2,$3,$4)", id, scope, slug, r.FormValue("name"))
		if e == nil {
			_, e = tx.Exec(r.Context(), "INSERT INTO template_versions(template_id,version,subject,html,plain) VALUES($1,1,'Your subject','<p>Your message</p>','Your message')", id)
		}
		if e == nil {
			e = tx.Commit(r.Context())
		}
		if e != nil {
			problem(w, 400, "template could not be created; ID must be unique")
			return
		}
		a.audit(r.Context(), person(r).ID, "template.create", scope, id)
		http.Redirect(w, r, "/templates/"+id, 303)
		return
	}
	if !a.require(w, r, scope, "template.view") {
		return
	}
	rows, _ := a.query(r.Context(), "SELECT t.*,v.subject,v.variables FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE t.application_id=$1 ORDER BY t.name", scope)
	a.render(w, r, "templates", map[string]any{"Rows": rows, "Scope": scope})
}
func regexpSlug(s string) bool {
	if len(s) < 1 || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}
func (a *App) templateEditor(w http.ResponseWriter, r *http.Request) {
	data, e := a.one(r.Context(), "SELECT t.*,v.subject,v.html,v.plain,v.variables,v.project FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE t.id=$1", r.PathValue("id"))
	if e != nil {
		problem(w, 404, "template not found")
		return
	}
	scope := data["application_id"].(string)
	if !a.require(w, r, scope, "template.edit") {
		return
	}
	if r.Method == "POST" {
		var vars map[string]string
		if e = json.Unmarshal([]byte(r.FormValue("variables")), &vars); e != nil {
			problem(w, 400, "test variables must be a JSON object of strings")
			return
		}
		_, _, e = a.Submit(r.Context(), SendInput{ApplicationID: scope, To: r.FormValue("to"), Template: r.PathValue("id"), Variables: vars}, newID("test_"))
		if e != nil {
			problem(w, 400, e.Error())
			return
		}
		http.Redirect(w, r, "/messages?app="+scope, 303)
		return
	}
	a.render(w, r, "editor", map[string]any{"Template": data, "Scope": scope})
}
func (a *App) editorData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, e := a.one(r.Context(), "SELECT t.*,v.subject,v.html,v.plain,v.variables,v.project FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE t.id=$1", id)
	if e != nil {
		problem(w, 404, "template not found")
		return
	}
	if !a.require(w, r, data["application_id"].(string), "template.edit") {
		return
	}
	if r.Method == "GET" {
		respond(w, 200, data)
		return
	}
	var in struct {
		Subject, HTML, Plain string
		Project              json.RawMessage
		Version              int
	}
	if e = json.NewDecoder(r.Body).Decode(&in); e != nil || len(in.HTML) > 2<<20 || len(in.Subject) > 998 || in.Subject == "" || strings.ContainsAny(in.Subject, "\r\n") {
		problem(w, 400, "invalid template content")
		return
	}
	if len(in.Project) == 0 {
		in.Project = json.RawMessage(`{}`)
	}
	if !json.Valid(in.Project) {
		problem(w, 400, "invalid editor project")
		return
	}
	if len(in.Project) > 2<<20 {
		problem(w, 400, "editor project exceeds 2 MiB")
		return
	}
	if in.Project, e = cleanProject(in.Project); e != nil {
		problem(w, 400, "invalid editor project")
		return
	}
	in.HTML = cleanHTML(in.HTML)
	if in.Plain == "" {
		in.Plain = plainHTML(in.HTML)
	}
	vars := []string{}
	for _, match := range placeholders.FindAllStringSubmatch(in.Subject+in.HTML+in.Plain, -1) {
		if !contains(vars, match[1]) {
			vars = append(vars, match[1])
		}
	}
	tx, e := a.DB.Begin(r.Context())
	if e != nil {
		problem(w, 500, "could not save template")
		return
	}
	defer tx.Rollback(r.Context())
	var current int
	e = tx.QueryRow(r.Context(), "SELECT current_version FROM templates WHERE id=$1 FOR UPDATE", id).Scan(&current)
	if e != nil {
		problem(w, 500, "could not load template version")
		return
	}
	if current != in.Version {
		problem(w, 409, "template changed in another session; reload before saving")
		return
	}
	_, e = tx.Exec(r.Context(), "INSERT INTO template_versions(template_id,version,subject,html,plain,variables,project) VALUES($1,$2,$3,$4,$5,$6,$7)", id, current+1, in.Subject, in.HTML, in.Plain, vars, []byte(in.Project))
	if e == nil {
		_, e = tx.Exec(r.Context(), "UPDATE templates SET current_version=$2 WHERE id=$1", id, current+1)
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		problem(w, 500, "template could not be saved")
		return
	}
	a.audit(r.Context(), person(r).ID, "template.version", data["application_id"].(string), id+":"+strconv.Itoa(current+1))
	respond(w, 200, map[string]any{"version": current + 1, "variables": vars})
}
func (a *App) access(w http.ResponseWriter, r *http.Request) {
	if !a.require(w, r, "*", "platform.manage") {
		return
	}
	if r.Method == "POST" {
		var e error
		action := r.FormValue("action")
		role, scope := r.FormValue("role"), r.FormValue("scope")
		if action == "binding" || action == "mapping" {
			if _, ok := roles[role]; !ok || role == "platform_admin" && scope != "*" || role != "platform_admin" && scope == "*" {
				problem(w, 400, "choose an application role or platform administrator")
				return
			}
			if scope != "*" {
				var exists bool
				a.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM applications WHERE id=$1)", scope).Scan(&exists)
				if !exists {
					problem(w, 400, "application not found")
					return
				}
			}
		}
		switch action {
		case "user":
			if len(r.FormValue("password")) < 12 {
				problem(w, 400, "password must contain at least 12 characters")
				return
			}
			hash, err := hashPassword(r.FormValue("password"))
			if err != nil {
				e = err
				break
			}
			username := strings.TrimSpace(r.FormValue("username"))
			if username == "" {
				problem(w, 400, "username required")
				return
			}
			_, e = a.DB.Exec(r.Context(), "INSERT INTO users(id,username,name,password_hash) VALUES($1,$2,$3,$4)", newID("usr_"), username, r.FormValue("name"), hash)
		case "password":
			if len(r.FormValue("password")) < 12 {
				problem(w, 400, "password must contain at least 12 characters")
				return
			}
			hash, err := hashPassword(r.FormValue("password"))
			if err != nil {
				e = err
				break
			}
			tag, err := a.DB.Exec(r.Context(), "UPDATE users SET password_hash=$2 WHERE id=$1 AND NOT root", r.FormValue("user"), hash)
			e = err
			if e == nil && tag.RowsAffected() == 0 {
				e = fmt.Errorf("use reset-root command for root credentials")
			}
			if e == nil {
				a.DB.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", r.FormValue("user"))
			}
		case "disable":
			tag, err := a.DB.Exec(r.Context(), "UPDATE users SET disabled=NOT disabled WHERE id=$1 AND NOT root", r.FormValue("user"))
			e = err
			if e == nil && tag.RowsAffected() == 0 {
				e = fmt.Errorf("root cannot be disabled")
			}
			if e == nil {
				a.DB.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", r.FormValue("user"))
			}
		case "binding":
			_, e = a.DB.Exec(r.Context(), "INSERT INTO bindings(user_id,scope,role) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", r.FormValue("user"), scope, role)
		case "mapping":
			if strings.TrimSpace(r.FormValue("group")) == "" {
				problem(w, 400, "group slug required")
				return
			}
			_, e = a.DB.Exec(r.Context(), "INSERT INTO group_mappings(group_slug,scope,role) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", strings.TrimSpace(r.FormValue("group")), scope, role)
		case "remove_binding":
			_, e = a.DB.Exec(r.Context(), "DELETE FROM bindings WHERE user_id=$1 AND scope=$2 AND role=$3", r.FormValue("user"), scope, role)
		case "remove_mapping":
			_, e = a.DB.Exec(r.Context(), "DELETE FROM group_mappings WHERE group_slug=$1 AND scope=$2 AND role=$3", r.FormValue("group"), scope, role)
		default:
			e = fmt.Errorf("unknown access action")
		}
		if e != nil {
			problem(w, 400, "access update failed: "+e.Error())
			return
		}
		a.audit(r.Context(), person(r).ID, "access."+action, scope, "")
		http.Redirect(w, r, "/access", 303)
		return
	}
	users, _ := a.query(r.Context(), "SELECT id,username,name,root,disabled,(password_hash<>'') AS local_login,groups FROM users ORDER BY name")
	bindings, _ := a.query(r.Context(), "SELECT b.*,u.name FROM bindings b JOIN users u ON u.id=b.user_id ORDER BY name")
	mappings, _ := a.query(r.Context(), "SELECT * FROM group_mappings ORDER BY group_slug")
	a.render(w, r, "access", map[string]any{"Users": users, "Bindings": bindings, "Mappings": mappings})
}
func (a *App) auditPage(w http.ResponseWriter, r *http.Request) {
	if !a.require(w, r, "*", "platform.manage") {
		return
	}
	rows, _ := a.query(r.Context(), "SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT 200")
	a.render(w, r, "audit", map[string]any{"Rows": rows})
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	if person(r) == nil {
		http.Redirect(w, r, "/login", 303)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		problem(w, 400, "search is too long")
		return
	}
	scope := r.URL.Query().Get("app")
	if scope != "" && !a.require(w, r, scope, "message.view") {
		return
	}
	apps := a.visibleApps(r)
	ids := []string{}
	for _, app := range apps {
		if (scope == "" || scope == app["id"].(string)) && a.Allowed(r.Context(), person(r), app["id"].(string), "message.view") {
			ids = append(ids, app["id"].(string))
		}
	}
	rows, e := a.query(r.Context(), "SELECT id,recipient,subject,class,status,attempts,created_at FROM messages WHERE application_id=ANY($1) AND ($2='' OR strpos(lower(recipient||' '||subject||' '||id),lower($2))>0) ORDER BY created_at DESC LIMIT 200", ids, q)
	if e != nil {
		problem(w, 500, "search unavailable")
		return
	}
	a.render(w, r, "messages", map[string]any{"Rows": rows, "Status": "", "Search": q, "Scope": scope})
}
