package edp

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func decodeAPI(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		problem(w, 400, "invalid JSON payload")
		return false
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		problem(w, 400, "expected one JSON object")
		return false
	}
	return true
}
func (a *App) apiScope(w http.ResponseWriter, r *http.Request, permission string) (string, bool) {
	p := person(r)
	if p == nil || p.KeyID == "" {
		problem(w, 401, "service API key required")
		return "", false
	}
	return p.Scope, a.require(w, r, p.Scope, permission)
}
func (a *App) keyActor(ctx context.Context, p *Principal) (string, error) {
	if p.KeyID == "" {
		return p.ID, nil
	}
	var id string
	e := a.DB.QueryRow(ctx, "SELECT k.created_by FROM api_keys k WHERE k.id=$1", p.KeyID).Scan(&id)
	return id, e
}
func (a *App) apiTemplates(w http.ResponseWriter, r *http.Request) {
	permission := "template.view"
	if r.Method == "POST" {
		permission = "template.edit"
	}
	app, ok := a.apiScope(w, r, permission)
	if !ok {
		return
	}
	if r.Method == "GET" {
		rows, e := a.query(r.Context(), "SELECT t.*,v.subject,v.variables FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE t.application_id=$1 AND ($2 OR NOT t.archived) ORDER BY t.name", app, r.URL.Query().Get("include_archived") == "true")
		if e != nil {
			problem(w, 500, "templates unavailable")
			return
		}
		respond(w, 200, map[string]any{"templates": rows})
		return
	}
	var in TemplateInput
	if !decodeAPI(w, r, &in) {
		return
	}
	id, e := a.createTemplate(r.Context(), app, in)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "template.create", app, id)
	respond(w, 201, map[string]any{"id": id, "version": 1})
}
func (a *App) apiTemplate(w http.ResponseWriter, r *http.Request) {
	permission := "template.view"
	if r.Method != "GET" {
		permission = "template.edit"
	}
	app, ok := a.apiScope(w, r, permission)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if r.Method == "GET" {
		version := positiveInt(r.URL.Query().Get("version"))
		row, e := a.one(r.Context(), "SELECT t.*,v.version,v.subject,v.html,v.plain,v.variables,v.project FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=CASE WHEN $3=0 THEN t.current_version ELSE $3 END WHERE t.id=$1 AND t.application_id=$2", id, app, version)
		if e != nil {
			problem(w, 404, "template not found")
			return
		}
		respond(w, 200, row)
		return
	}
	var in TemplateInput
	if !decodeAPI(w, r, &in) {
		return
	}
	v, vars, e := a.saveTemplateVersion(r.Context(), app, id, in)
	if e != nil {
		status := 400
		if strings.Contains(e.Error(), "another session") {
			status = 409
		}
		problem(w, status, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "template.version", app, id)
	respond(w, 200, map[string]any{"id": id, "version": v, "variables": vars})
}
func (a *App) apiCampaigns(w http.ResponseWriter, r *http.Request) {
	permission := "campaign.view"
	if r.Method == "POST" {
		permission = "campaign.edit"
	}
	app, ok := a.apiScope(w, r, permission)
	if !ok {
		return
	}
	if r.Method == "GET" {
		rows, e := a.query(r.Context(), "SELECT id,name,kind,state,approved_at,scheduled_at,created_at FROM campaigns WHERE application_id=$1 ORDER BY created_at DESC LIMIT 1000", app)
		if e != nil {
			problem(w, 500, "campaigns unavailable")
			return
		}
		respond(w, 200, map[string]any{"campaigns": rows})
		return
	}
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if !decodeAPI(w, r, &in) {
		return
	}
	if in.Kind == "" {
		in.Kind = "certificate"
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 || !contains([]string{"certificate", "bulk"}, in.Kind) {
		problem(w, 400, "name and certificate or bulk kind required")
		return
	}
	actor, e := a.keyActor(r.Context(), person(r))
	if e != nil {
		problem(w, 400, "create a new service key before using campaign automation")
		return
	}
	id := newID("cmp_")
	_, e = a.DB.Exec(r.Context(), "INSERT INTO campaigns(id,application_id,name,kind,created_by) VALUES($1,$2,$3,$4,$5)", id, app, in.Name, in.Kind, actor)
	if e != nil {
		problem(w, 500, "campaign could not be created")
		return
	}
	a.audit(r.Context(), person(r).ID, "campaign.create", app, id)
	respond(w, 201, map[string]any{"id": id, "state": "draft"})
}
func (a *App) scopedCampaign(w http.ResponseWriter, r *http.Request, permission string) (string, map[string]any, bool) {
	app, ok := a.apiScope(w, r, permission)
	if !ok {
		return "", nil, false
	}
	row, e := a.one(r.Context(), "SELECT * FROM campaigns WHERE id=$1 AND application_id=$2", r.PathValue("id"), app)
	if e != nil {
		problem(w, 404, "campaign not found")
		return "", nil, false
	}
	return app, row, true
}
func (a *App) apiCampaign(w http.ResponseWriter, r *http.Request) {
	_, row, ok := a.scopedCampaign(w, r, "campaign.view")
	if !ok {
		return
	}
	rows, e := a.query(r.Context(), "SELECT r.*,t.slug AS template_slug FROM campaign_rows r LEFT JOIN templates t ON t.id=r.template_id WHERE r.campaign_id=$1 ORDER BY ordinal LIMIT 5000", r.PathValue("id"))
	if e != nil {
		problem(w, 500, "campaign rows unavailable")
		return
	}
	row["rows"] = rows
	respond(w, 200, row)
}
func (a *App) apiCampaignImport(w http.ResponseWriter, r *http.Request) {
	app, _, ok := a.scopedCampaign(w, r, "campaign.edit")
	if !ok {
		return
	}
	var rows [][]string
	var e error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if e = r.ParseMultipartForm(1 << 20); e == nil {
			var f io.ReadCloser
			var filename string
			f, h, err := r.FormFile("file")
			e = err
			if e == nil {
				defer f.Close()
				filename = h.Filename
				rows, e = parseImport(f, filename, r.FormValue("sheet"))
			}
		}
	} else {
		var in struct {
			Rows     [][]string `json:"rows"`
			SheetURL string     `json:"sheet_url"`
		}
		if !decodeAPI(w, r, &in) {
			return
		}
		if in.SheetURL != "" {
			rows, e = a.sheetRows(r.Context(), in.SheetURL)
		} else {
			rows, e = normalizeImportRows(in.Rows)
		}
	}
	if e == nil {
		e = a.replaceCampaignSource(r.Context(), r.PathValue("id"), rows)
	}
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "campaign.import", app, r.PathValue("id"))
	respond(w, 200, map[string]any{"rows": len(rows) - 1, "headers": rows[0]})
}
func (a *App) apiWorkbookSheets(w http.ResponseWriter, r *http.Request) {
	_, ok := a.apiScope(w, r, "campaign.edit")
	if !ok {
		return
	}
	a.workbookSheets(w, r)
}
func (a *App) workbookSheets(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		if !a.require(w, r, r.PathValue("id"), "campaign.edit") {
			return
		}
	}
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		problem(w, 400, "invalid workbook upload")
		return
	}
	f, _, e := r.FormFile("file")
	if e != nil {
		problem(w, 400, "choose a workbook")
		return
	}
	defer f.Close()
	sheets, e := xlsxSheets(f)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	respond(w, 200, map[string]any{"sheets": sheets})
}
func (a *App) apiCampaignMapping(w http.ResponseWriter, r *http.Request) {
	app, row, ok := a.scopedCampaign(w, r, "campaign.edit")
	if !ok {
		return
	}
	var e error
	if row["kind"] == "bulk" {
		var in BulkMapping
		if !decodeAPI(w, r, &in) {
			return
		}
		e = a.mapBulkCampaign(r.Context(), r.PathValue("id"), in.Template, in.Email, in.Columns, in.Constants)
	} else {
		in := Mapping{Email: -1, School: -1, Event: -1, Award: -1, URL: -1}
		if !decodeAPI(w, r, &in) {
			return
		}
		e = a.mapCampaign(r.Context(), r.PathValue("id"), in)
	}
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "campaign.map", app, r.PathValue("id"))
	respond(w, 200, map[string]any{"mapped": true})
}
func (a *App) apiCampaignAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	permission := "campaign.edit"
	if action == "approve" {
		permission = "campaign.approve"
	}
	if contains([]string{"start", "pause", "resume", "cancel"}, action) {
		permission = "campaign.send"
	}
	app, _, ok := a.scopedCampaign(w, r, permission)
	if !ok {
		return
	}
	var in CampaignAction
	if !decodeAPI(w, r, &in) {
		return
	}
	actor, e := a.keyActor(r.Context(), person(r))
	if e != nil {
		problem(w, 400, "create a new service key before using campaign automation")
		return
	}
	if e == nil {
		e = a.actCampaign(r.Context(), r.PathValue("id"), app, actor, action, in)
	}
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "campaign."+action, app, r.PathValue("id"))
	respond(w, 200, map[string]any{"success": true})
}

type CampaignAction struct {
	ScheduleAt *time.Time `json:"schedule_at"`
	To         string     `json:"to"`
	RowID      string     `json:"row_id"`
	Excluded   bool       `json:"excluded"`
}

func (a *App) actCampaign(ctx context.Context, id, app, actor, action string, in CampaignAction) error {
	switch action {
	case "approve":
		return a.approveCampaign(ctx, id, actor)
	case "start":
		when := time.Now()
		if in.ScheduleAt != nil {
			when = *in.ScheduleAt
		}
		return a.startCampaign(ctx, id, when)
	case "draft", "pause", "resume":
		from, to := "approved", "draft"
		if action == "pause" {
			from, to = "sending", "paused"
		}
		if action == "resume" {
			from, to = "paused", "sending"
		}
		sql := "UPDATE campaigns SET state=$3 WHERE id=$1 AND state=$2"
		if action == "draft" {
			sql = "UPDATE campaigns SET state=$3,approved_by=NULL,approved_at=NULL WHERE id=$1 AND state=$2"
		}
		tag, e := a.DB.Exec(ctx, sql, id, from, to)
		if e == nil && tag.RowsAffected() != 1 {
			e = fmt.Errorf("campaign state does not allow this action")
		}
		return e
	case "cancel":
		tx, e := a.DB.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		tag, e := tx.Exec(ctx, "UPDATE campaigns SET state='cancelled' WHERE id=$1 AND state IN('draft','approved','sending','paused')", id)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("campaign cannot be cancelled")
		}
		_, e = tx.Exec(ctx, `WITH changed AS(UPDATE messages SET status='cancelled',updated_at=now() WHERE campaign_id=$1 AND status IN('queued','retry_wait') RETURNING id) INSERT INTO delivery_events(message_id,status) SELECT id,'cancelled' FROM changed`, id)
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	case "exclude":
		return a.setCampaignRowExcluded(ctx, id, in.RowID, &in.Excluded)
	case "test":
		var tid string
		var v int
		var data []byte
		e := a.DB.QueryRow(ctx, "SELECT template_id,template_version,variables FROM campaign_rows WHERE campaign_id=$1 AND NOT excluded AND cardinality(errors)=0 ORDER BY ordinal LIMIT 1", id).Scan(&tid, &v, &data)
		if e != nil {
			return fmt.Errorf("no valid included rows")
		}
		var vars map[string]string
		if e = json.Unmarshal(data, &vars); e != nil {
			return e
		}
		_, _, e = a.Submit(ctx, SendInput{ApplicationID: app, To: in.To, Template: tid, Version: v, Variables: vars}, newID("test_"))
		return e
	default:
		return fmt.Errorf("unknown campaign action")
	}
}

type BulkMapping struct {
	Template  string            `json:"template"`
	Email     int               `json:"email"`
	Columns   map[string]int    `json:"columns"`
	Constants map[string]string `json:"constants"`
}

func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r\n", rune(s[0])) {
		return "'" + s
	}
	return s
}
func (a *App) campaignExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var app string
	if e := a.DB.QueryRow(r.Context(), "SELECT application_id FROM campaigns WHERE id=$1", id).Scan(&app); e != nil {
		problem(w, 404, "campaign not found")
		return
	}
	if !a.require(w, r, app, "campaign.view") {
		return
	}
	rows, e := a.DB.Query(r.Context(), `SELECT r.ordinal,r.recipient,coalesce(t.slug,''),r.excluded,r.errors,coalesce(m.id,''),coalesce(m.status,''),coalesce(m.attempts,0),coalesce(m.last_error,''),r.source_ordinals FROM campaign_rows r LEFT JOIN templates t ON t.id=r.template_id LEFT JOIN messages m ON m.campaign_id=r.campaign_id AND m.idempotency_key='campaign:'||r.campaign_id||':'||r.dispatch_key WHERE r.campaign_id=$1 ORDER BY r.ordinal`, id)
	if e != nil {
		problem(w, 500, "export unavailable")
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="campaign-results.csv"`)
	w.Write([]byte{0xef, 0xbb, 0xbf})
	out := csv.NewWriter(w)
	out.Write([]string{"source_row", "recipient", "template", "excluded", "validation_errors", "message_id", "status", "attempts", "delivery_detail", "source_rows", "certificate_count"})
	for rows.Next() {
		var ordinal, attempts int
		var recipient, slug, mid, status, detail string
		var excluded bool
		var errs []string
		var sources []int
		if e = rows.Scan(&ordinal, &recipient, &slug, &excluded, &errs, &mid, &status, &attempts, &detail, &sources); e != nil {
			break
		}
		if len(sources) == 0 {
			sources = []int{ordinal}
		}
		labels := make([]string, len(sources))
		for i, source := range sources {
			labels[i] = strconv.Itoa(source)
		}
		record := []string{strconv.Itoa(ordinal), recipient, slug, strconv.FormatBool(excluded), strings.Join(errs, "; "), mid, status, strconv.Itoa(attempts), detail, strings.Join(labels, "; "), strconv.Itoa(len(sources))}
		for i := range record {
			record[i] = csvSafe(record[i])
		}
		out.Write(record)
	}
	out.Flush()
}
func (a *App) apiCampaignProgress(w http.ResponseWriter, r *http.Request) {
	_, row, ok := a.scopedCampaign(w, r, "campaign.view")
	if !ok {
		return
	}
	counts, e := a.query(r.Context(), "SELECT status,count(*) AS count FROM messages WHERE campaign_id=$1 GROUP BY status", r.PathValue("id"))
	if e != nil {
		problem(w, 500, "progress unavailable")
		return
	}
	respond(w, 200, map[string]any{"id": row["id"], "state": row["state"], "statuses": counts})
}
func (a *App) apiWebhooks(w http.ResponseWriter, r *http.Request) {
	permission := "webhook.manage"
	app, ok := a.apiScope(w, r, permission)
	if !ok {
		return
	}
	if r.Method == "GET" {
		rows, e := a.listWebhooks(r.Context(), app)
		if e != nil {
			problem(w, 500, "webhooks unavailable")
			return
		}
		respond(w, 200, map[string]any{"webhooks": rows})
		return
	}
	var in struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Secret  string `json:"secret"`
		Enabled bool   `json:"enabled"`
	}
	if !decodeAPI(w, r, &in) {
		return
	}
	id, e := a.saveWebhook(r.Context(), app, r.PathValue("id"), in.Name, in.URL, in.Secret, in.Enabled)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "webhook.save", app, id)
	respond(w, 200, map[string]any{"id": id})
}

var serviceScopes = []string{"message.send", "message.otp", "message.view", "message.cancel", "delivery.report", "template.view", "template.edit", "campaign.view", "campaign.edit", "campaign.approve", "campaign.send", "webhook.manage", "relay.manage"}

func (a *App) webhookView(ctx context.Context, app string) []map[string]any {
	rows, _ := a.listWebhooks(ctx, app)
	return rows
}
func (a *App) hasNamedFallback(ctx context.Context) bool {
	var yes bool
	a.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM smtp_relays WHERE enabled AND fallback_id IS NOT NULL)").Scan(&yes)
	return yes
}

func (a *App) grantableScopes(r *http.Request) []string {
	var out []string
	for _, scope := range serviceScopes {
		allowed := a.Allowed(r.Context(), person(r), r.PathValue("id"), scope)
		if scope == "message.otp" {
			allowed = a.Allowed(r.Context(), person(r), r.PathValue("id"), "message.send")
		}
		if scope == "delivery.report" || scope == "relay.manage" {
			allowed = a.Allowed(r.Context(), person(r), "*", "platform.manage")
		}
		if allowed {
			out = append(out, scope)
		}
	}
	return out
}
