package edp

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"golang.org/x/oauth2/google"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func parseCSV(reader io.Reader) ([][]string, error) {
	data, e := io.ReadAll(io.LimitReader(reader, (8<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(data) > 8<<20 {
		return nil, fmt.Errorf("CSV exceeds the 8 MiB import limit")
	}
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	rows, e := r.ReadAll()
	if e != nil {
		return nil, e
	}
	if len(rows) < 2 || len(rows) > 5001 {
		return nil, fmt.Errorf("CSV must contain a header and 1–5000 data rows")
	}
	rows[0][0] = strings.TrimPrefix(rows[0][0], "\ufeff")
	for _, row := range rows {
		if len(row) > 52 {
			return nil, fmt.Errorf("maximum 52 columns")
		}
	}
	return rows, nil
}
func (a *App) sheetRows(ctx context.Context, link string) ([][]string, error) {
	if a.C.GoogleCredentials == "" {
		return nil, fmt.Errorf("Google Sheets import requires GOOGLE_CREDENTIALS_FILE and read access to the sheet")
	}
	u, e := url.Parse(link)
	if e != nil || u.Scheme != "https" || u.Host != "docs.google.com" {
		return nil, fmt.Errorf("use a Google Sheets HTTPS URL")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 4 || parts[1] != "spreadsheets" || parts[2] != "d" {
		return nil, fmt.Errorf("invalid Google Sheets URL")
	}
	id := parts[3]
	if len(id) > 150 || strings.ContainsAny(id, "?&#") {
		return nil, fmt.Errorf("invalid spreadsheet ID")
	}
	data, e := os.ReadFile(a.C.GoogleCredentials)
	if e != nil {
		return nil, fmt.Errorf("cannot read Google credentials")
	}
	conf, e := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/spreadsheets.readonly")
	if e != nil {
		return nil, fmt.Errorf("expected Google service-account credentials")
	}
	client := conf.Client(ctx)
	client.Timeout = 15 * time.Second
	get := func(path string, target any) error {
		req, e := http.NewRequestWithContext(ctx, "GET", "https://sheets.googleapis.com/v4/spreadsheets/"+url.PathEscape(id)+path, nil)
		if e != nil {
			return e
		}
		res, e := client.Do(req)
		if e != nil {
			return e
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("Google Sheets returned %d; share this sheet with the configured service account", res.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(target)
	}
	var meta struct {
		Sheets []struct {
			Properties struct {
				SheetID int    `json:"sheetId"`
				Title   string `json:"title"`
				Grid    struct {
					Rows    int `json:"rowCount"`
					Columns int `json:"columnCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if e = get("?fields=sheets(properties(sheetId,title,gridProperties))", &meta); e != nil {
		return nil, e
	}
	if len(meta.Sheets) == 0 {
		return nil, fmt.Errorf("spreadsheet has no tabs")
	}
	gid := u.Query().Get("gid")
	if gid == "" {
		v, _ := url.ParseQuery(u.Fragment)
		gid = v.Get("gid")
	}
	selected := 0
	if gid != "" {
		selected = -1
		for i, s := range meta.Sheets {
			if strconv.Itoa(s.Properties.SheetID) == gid {
				selected = i
			}
		}
		if selected < 0 {
			return nil, fmt.Errorf("requested sheet tab does not exist")
		}
	}
	s := meta.Sheets[selected].Properties
	rows := min(s.Grid.Rows, 5002)
	cols := min(s.Grid.Columns, 52)
	if cols < 1 || rows < 2 {
		return nil, fmt.Errorf("sheet is empty")
	}
	col := ""
	for n := cols; n > 0; n = (n - 1) / 26 {
		col = string(rune('A'+(n-1)%26)) + col
	}
	rangeName := "'" + strings.ReplaceAll(s.Title, "'", "''") + "'!A1:" + col + strconv.Itoa(rows)
	var result struct {
		Values [][]any `json:"values"`
	}
	if e = get("/values/"+url.PathEscape(rangeName)+"?valueRenderOption=FORMATTED_VALUE", &result); e != nil {
		return nil, e
	}
	out := make([][]string, 0, len(result.Values))
	for _, row := range result.Values {
		v := make([]string, len(row))
		for i, x := range row {
			v[i] = fmt.Sprint(x)
		}
		out = append(out, v)
	}
	if len(out) > 5001 {
		return nil, fmt.Errorf("sheet contains more than 5000 data rows; split the campaign before importing")
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("selected sheet has no data")
	}
	return out, nil
}

type Mapping struct {
	Email, School, Event, Award, URL int
	DefaultEvent, DefaultAward       string
}

func cell(row []string, n int) string {
	if n < 0 || n >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[n])
}
func (a *App) mapCampaign(ctx context.Context, id string, m Mapping) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var app, state string
	var data []byte
	e = tx.QueryRow(ctx, "SELECT application_id,state,source_rows FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&app, &state, &data)
	if e != nil {
		return e
	}
	if state != "draft" {
		return fmt.Errorf("only draft campaigns can be mapped")
	}
	var rows [][]string
	if e = json.Unmarshal(data, &rows); e != nil {
		return e
	}
	if len(rows) < 2 {
		return fmt.Errorf("import data first")
	}
	if _, e = tx.Exec(ctx, "DELETE FROM campaign_rows WHERE campaign_id=$1", id); e != nil {
		return e
	}
	seen := map[string]bool{}
	for i, row := range rows[1:] {
		email, school, event, award, link := cell(row, m.Email), cell(row, m.School), cell(row, m.Event), cell(row, m.Award), cell(row, m.URL)
		if event == "" {
			event = m.DefaultEvent
		}
		if award == "" {
			award = m.DefaultAward
		}
		var errs []string
		if !validAddress(email) {
			errs = append(errs, "Invalid recipient email")
		}
		if school == "" {
			errs = append(errs, "School name is missing")
		}
		u, err := url.Parse(link)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, "Certificate link must be an HTTPS URL")
		}
		var tpl *string
		var version *int
		var tid string
		var v int
		err = tx.QueryRow(ctx, "SELECT id,current_version FROM templates WHERE application_id=$1 AND slug=$2", app, event+"."+award).Scan(&tid, &v)
		if err != nil {
			errs = append(errs, "No template for event/award: "+event+"/"+award)
		} else {
			tpl = &tid
			version = &v
		}
		vars := map[string]string{"school_name": school, "certificate_url": link}
		dispatch := tokenHash(strings.ToLower(email) + "|" + school + "|" + event + "|" + award + "|" + link)
		if seen[dispatch] {
			errs = append(errs, "Duplicate certificate dispatch")
		}
		seen[dispatch] = true
		if errs == nil {
			errs = []string{}
		}
		data, _ := json.Marshal(vars)
		if _, e = tx.Exec(ctx, "INSERT INTO campaign_rows(id,campaign_id,ordinal,recipient,template_id,template_version,variables,errors,dispatch_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", newID("row_"), id, i+2, email, tpl, version, data, errs, dispatch); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (a *App) approveCampaign(ctx context.Context, id, actor string) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var state string
	if e = tx.QueryRow(ctx, "SELECT state FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&state); e != nil {
		return e
	}
	if state != "draft" {
		return fmt.Errorf("campaign must be a draft")
	}
	var n, invalid int
	e = tx.QueryRow(ctx, "SELECT count(*) FILTER(WHERE NOT excluded),count(*) FILTER(WHERE NOT excluded AND cardinality(errors)>0) FROM campaign_rows WHERE campaign_id=$1", id).Scan(&n, &invalid)
	if e != nil {
		return e
	}
	if n == 0 || invalid > 0 {
		return fmt.Errorf("include valid rows and exclude or correct every invalid row before approval")
	}
	_, e = tx.Exec(ctx, "UPDATE campaigns SET state='approved',approved_by=$2,approved_at=now() WHERE id=$1", id, actor)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) startCampaign(ctx context.Context, id string, scheduled time.Time) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var app, state string
	e = tx.QueryRow(ctx, "SELECT application_id,state FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&app, &state)
	if e != nil {
		return e
	}
	if state != "approved" {
		return fmt.Errorf("approve campaign before scheduling")
	}
	var from, reply string
	var daily, queueLimit, recipientLimit int
	var paused bool
	e = tx.QueryRow(ctx, "SELECT from_address,reply_to,daily_limit,queue_limit,recipient_hour_limit,paused FROM applications WHERE id=$1 FOR UPDATE", app).Scan(&from, &reply, &daily, &queueLimit, &recipientLimit, &paused)
	if e != nil {
		return e
	}
	var platformPaused bool
	tx.QueryRow(ctx, "SELECT paused FROM platform_settings WHERE id=1").Scan(&platformPaused)
	if paused || platformPaused {
		return fmt.Errorf("sending is paused")
	}
	var count, used, queued int
	tx.QueryRow(ctx, "SELECT count(*) FROM campaign_rows WHERE campaign_id=$1 AND NOT excluded", id).Scan(&count)
	tx.QueryRow(ctx, "SELECT count(*) FILTER(WHERE created_at>=now()-interval '24 hours'),count(*) FILTER(WHERE status IN('queued','retry_wait','sending')) FROM messages WHERE application_id=$1", app).Scan(&used, &queued)
	if count+used > daily*9/10 || count+queued > queueLimit*8/10 {
		return fmt.Errorf("campaign exceeds application quota or queue capacity; transactional capacity is reserved")
	}
	var overRecipient bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM campaign_rows r WHERE campaign_id=$1 AND NOT excluded GROUP BY recipient HAVING count(*)+(SELECT count(*) FROM messages m WHERE m.application_id=$2 AND m.recipient=r.recipient AND m.created_at>=now()-interval '1 hour')>$3)`, id, app, recipientLimit).Scan(&overRecipient)
	if e != nil {
		return e
	}
	if overRecipient {
		return fmt.Errorf("campaign exceeds recipient cooldown limits")
	}
	if scheduled.Before(time.Now()) {
		scheduled = time.Now()
	}
	if scheduled.After(time.Now().Add(23 * 24 * time.Hour)) {
		return fmt.Errorf("schedule must be within 23 days")
	}
	rows, e := tx.Query(ctx, "SELECT recipient,template_id,template_version,variables,dispatch_key FROM campaign_rows WHERE campaign_id=$1 AND NOT excluded ORDER BY ordinal", id)
	if e != nil {
		return e
	}
	type item struct {
		To, Template, Key string
		Version           int
		Vars              map[string]string
	}
	var items []item
	for rows.Next() {
		var i item
		var data []byte
		if e = rows.Scan(&i.To, &i.Template, &i.Version, &data, &i.Key); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(data, &i.Vars); e != nil {
			rows.Close()
			return e
		}
		items = append(items, i)
	}
	rows.Close()
	for _, i := range items {
		rendered, e := renderMessageWith(ctx, tx, app, i.Template, i.Version, i.Vars)
		if e != nil {
			return e
		}
		mid := newID("msg_")
		_, e = tx.Exec(ctx, `INSERT INTO messages(id,application_id,campaign_id,recipient,from_address,reply_to,subject,html,plain,class,idempotency_key,payload_hash,available_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'bulk',$10,$11,$12,$13)`, mid, app, id, i.To, from, reply, rendered.Subject, rendered.HTML, rendered.Plain, "campaign:"+id+":"+i.Key, i.Key, scheduled, scheduled.Add(7*24*time.Hour))
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO delivery_events(message_id,status) VALUES($1,'queued')", mid); e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, "UPDATE campaigns SET state='sending',scheduled_at=$2 WHERE id=$1", id, scheduled)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) campaigns(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("app")
	if r.Method == "POST" {
		r.ParseForm()
		scope = r.FormValue("app")
		if !a.require(w, r, scope, "campaign.edit") {
			return
		}
		id := newID("cmp_")
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			problem(w, 400, "campaign name required")
			return
		}
		kind := r.FormValue("kind")
		if kind == "" {
			kind = "certificate"
		}
		if kind != "bulk" && kind != "certificate" {
			problem(w, 400, "unknown campaign type")
			return
		}
		_, e := a.DB.Exec(r.Context(), "INSERT INTO campaigns(id,application_id,name,created_by,kind) VALUES($1,$2,$3,$4,$5)", id, scope, name, person(r).ID, kind)
		if e != nil {
			problem(w, 500, "could not create campaign")
			return
		}
		a.audit(r.Context(), person(r).ID, "campaign.create", scope, id)
		http.Redirect(w, r, "/campaigns/"+id, 303)
		return
	}
	if !a.require(w, r, scope, "campaign.view") {
		return
	}
	rows, e := a.query(r.Context(), `SELECT c.*,count(m.id) AS total,count(m.id) FILTER(WHERE m.status IN('relay_accepted','remote_accepted')) AS accepted FROM campaigns c LEFT JOIN messages m ON m.campaign_id=c.id WHERE c.application_id=$1 GROUP BY c.id ORDER BY c.created_at DESC`, scope)
	if e != nil {
		problem(w, 500, "could not load campaigns")
		return
	}
	a.render(w, r, "campaigns", map[string]any{"Rows": rows, "Scope": scope})
}
func (a *App) campaign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, e := a.one(r.Context(), "SELECT * FROM campaigns WHERE id=$1", id)
	if e != nil {
		problem(w, 404, "campaign not found")
		return
	}
	scope := data["application_id"].(string)
	permission := "campaign.view"
	if r.Method == "POST" {
		switch r.FormValue("action") {
		case "approve":
			permission = "campaign.approve"
		case "start", "pause", "resume", "cancel":
			permission = "campaign.send"
		default:
			permission = "campaign.edit"
		}
	}
	if !a.require(w, r, scope, permission) {
		return
	}
	if r.Method == "POST" {
		action := r.FormValue("action")
		switch action {
		case "import":
			if data["state"] != "draft" {
				e = fmt.Errorf("return campaign to draft before importing")
				break
			}
			var rows [][]string
			if r.FormValue("sheet_url") != "" {
				rows, e = a.sheetRows(r.Context(), r.FormValue("sheet_url"))
			} else {
				file, _, err := r.FormFile("csv")
				if err != nil {
					e = fmt.Errorf("choose a CSV file or provide a sheet URL")
					break
				}
				defer file.Close()
				rows, e = parseCSV(file)
			}
			if e == nil {
				e = a.replaceCampaignSource(r.Context(), id, rows)
			}

		case "map":
			parse := func(k string) int {
				n, err := strconv.Atoi(r.FormValue(k))
				if err != nil {
					return -1
				}
				return n
			}
			if data["kind"] == "bulk" {
				columns := map[string]int{}
				constants := map[string]string{}
				for key := range r.Form {
					if strings.HasPrefix(key, "var.") {
						columns[strings.TrimPrefix(key, "var.")] = parse(key)
					}
					if strings.HasPrefix(key, "const.") {
						constants[strings.TrimPrefix(key, "const.")] = r.FormValue(key)
					}
				}
				e = a.mapBulkCampaign(r.Context(), id, r.FormValue("template"), parse("email"), columns, constants)
			} else {
				e = a.mapCampaign(r.Context(), id, Mapping{parse("email"), parse("school"), parse("event"), parse("award"), parse("url"), r.FormValue("default_event"), r.FormValue("default_award")})
			}
		case "exclude":
			e = a.toggleCampaignRow(r.Context(), id, r.FormValue("row"))

		case "approve":
			e = a.approveCampaign(r.Context(), id, person(r).ID)
		case "draft":
			if data["state"] != "approved" {
				e = fmt.Errorf("only an approved, unsent campaign can return to draft")
			} else {
				_, e = a.DB.Exec(r.Context(), "UPDATE campaigns SET state='draft',approved_by=NULL,approved_at=NULL WHERE id=$1 AND state='approved'", id)
			}
		case "start":
			when := time.Now()
			if v := r.FormValue("schedule"); v != "" {
				zone := time.FixedZone("Asia/Bangkok", 7*3600)
				when, e = time.ParseInLocation("2006-01-02T15:04", v, zone)
			}
			if e == nil {
				e = a.startCampaign(r.Context(), id, when)
			}
		case "pause":
			_, e = a.DB.Exec(r.Context(), "UPDATE campaigns SET state='paused' WHERE id=$1 AND state='sending'", id)
		case "resume":
			_, e = a.DB.Exec(r.Context(), "UPDATE campaigns SET state='sending' WHERE id=$1 AND state='paused'", id)
		case "cancel":
			tx, err := a.DB.Begin(r.Context())
			if err != nil {
				e = err
				break
			}
			defer tx.Rollback(r.Context())
			_, e = tx.Exec(r.Context(), "UPDATE campaigns SET state='cancelled' WHERE id=$1 AND state IN('draft','approved','sending','paused')", id)
			if e == nil {
				_, e = tx.Exec(r.Context(), `WITH changed AS(UPDATE messages SET status='cancelled',updated_at=now() WHERE campaign_id=$1 AND status IN('queued','retry_wait') RETURNING id) INSERT INTO delivery_events(message_id,status) SELECT id,'cancelled' FROM changed`, id)
			}
			if e == nil {
				e = tx.Commit(r.Context())
			}
		case "test":
			var recipient, template string
			var version int
			var varsJSON []byte
			e = a.DB.QueryRow(r.Context(), "SELECT recipient,template_id,template_version,variables FROM campaign_rows WHERE campaign_id=$1 AND NOT excluded AND cardinality(errors)=0 ORDER BY ordinal LIMIT 1", id).Scan(&recipient, &template, &version, &varsJSON)
			if e == nil {
				var vars map[string]string
				json.Unmarshal(varsJSON, &vars)
				_, _, e = a.Submit(r.Context(), SendInput{ApplicationID: scope, To: r.FormValue("test_to"), Template: template, Version: version, Variables: vars, Class: "transactional"}, newID("test_"))
			}
		default:
			e = fmt.Errorf("unknown campaign action")
		}
		if e != nil {
			problem(w, 400, e.Error())
			return
		}
		a.audit(r.Context(), person(r).ID, "campaign."+action, scope, id)
		http.Redirect(w, r, "/campaigns/"+id, 303)
		return
	}
	rows, e := a.query(r.Context(), "SELECT r.*,t.name AS template_name FROM campaign_rows r LEFT JOIN templates t ON t.id=r.template_id WHERE campaign_id=$1 ORDER BY ordinal LIMIT 5000", id)
	if e != nil {
		problem(w, 500, "could not load campaign rows")
		return
	}
	var source [][]string
	raw, _ := json.Marshal(data["source_rows"])
	json.Unmarshal(raw, &source)
	var headers []string
	if len(source) > 0 {
		headers = source[0]
	}
	templates, _ := a.query(r.Context(), "SELECT t.id,t.name,v.variables FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE application_id=$1 ORDER BY name", scope)
	selected := r.URL.Query().Get("template")
	if selected == "" && len(templates) > 0 {
		selected = templates[0]["id"].(string)
	}
	var variables []string
	for _, t := range templates {
		if t["id"] == selected {
			if names, ok := t["variables"].([]any); ok {
				for _, n := range names {
					variables = append(variables, fmt.Sprint(n))
				}
			} else if names, ok := t["variables"].([]string); ok {
				variables = names
			}
		}
	}
	a.render(w, r, "campaign", map[string]any{"Campaign": data, "Rows": rows, "Headers": headers, "Scope": scope, "SheetConfigured": a.C.GoogleCredentials != "", "Templates": templates, "SelectedTemplate": selected, "TemplateVariables": variables})
}
func (a *App) campaignPreview(w http.ResponseWriter, r *http.Request) {
	var scope, tid string
	var v int
	var data []byte
	e := a.DB.QueryRow(r.Context(), "SELECT c.application_id,r.template_id,r.template_version,r.variables FROM campaign_rows r JOIN campaigns c ON c.id=r.campaign_id WHERE r.id=$1", r.PathValue("row")).Scan(&scope, &tid, &v, &data)
	if e != nil {
		problem(w, 404, "row not found")
		return
	}
	if !a.require(w, r, scope, "campaign.view") {
		return
	}
	var vars map[string]string
	json.Unmarshal(data, &vars)
	rendered, e := a.renderMessage(r.Context(), scope, tid, v, vars)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src https: http: data:; style-src 'unsafe-inline'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(rendered.HTML))
}

func (a *App) campaignProgress(w http.ResponseWriter, r *http.Request) {
	var scope, state string
	e := a.DB.QueryRow(r.Context(), "SELECT application_id,state FROM campaigns WHERE id=$1", r.PathValue("id")).Scan(&scope, &state)
	if e != nil {
		problem(w, 404, "campaign not found")
		return
	}
	if !a.require(w, r, scope, "campaign.view") {
		return
	}
	counts, e := a.query(r.Context(), "SELECT status,count(*) AS count FROM messages WHERE campaign_id=$1 GROUP BY status ORDER BY status", r.PathValue("id"))
	if e != nil {
		problem(w, 500, "progress unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<div><strong>%s</strong>", state)
	for _, count := range counts {
		fmt.Fprintf(w, "<span class=\"status ms-2\">%s: %v</span>", count["status"], count["count"])
	}
	fmt.Fprint(w, "</div>")
}

func (a *App) replaceCampaignSource(ctx context.Context, id string, rows [][]string) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var state string
	if e = tx.QueryRow(ctx, "SELECT state FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&state); e != nil {
		return e
	}
	if state != "draft" {
		return fmt.Errorf("only draft campaigns may be imported")
	}
	raw, _ := json.Marshal(rows)
	if _, e = tx.Exec(ctx, "UPDATE campaigns SET source_rows=$2 WHERE id=$1", id, raw); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM campaign_rows WHERE campaign_id=$1", id); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) toggleCampaignRow(ctx context.Context, id, row string) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var state string
	if e = tx.QueryRow(ctx, "SELECT state FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&state); e != nil {
		return e
	}
	if state != "draft" {
		return fmt.Errorf("only draft rows may change")
	}
	if _, e = tx.Exec(ctx, "UPDATE campaign_rows SET excluded=NOT excluded WHERE campaign_id=$1 AND id=$2", id, row); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (a *App) mapBulkCampaign(ctx context.Context, id, templateID string, emailColumn int, columns map[string]int, constants map[string]string) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var scope, state string
	var data []byte
	if e = tx.QueryRow(ctx, "SELECT application_id,state,source_rows FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&scope, &state, &data); e != nil {
		return e
	}
	if state != "draft" {
		return fmt.Errorf("only draft campaigns may change")
	}
	var version int
	var required []string
	e = tx.QueryRow(ctx, "SELECT t.current_version,v.variables FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE t.id=$1 AND t.application_id=$2", templateID, scope).Scan(&version, &required)
	if e != nil {
		return fmt.Errorf("select a template belonging to this application")
	}
	var rows [][]string
	if e = json.Unmarshal(data, &rows); e != nil {
		return e
	}
	if len(rows) < 2 {
		return fmt.Errorf("import recipients first")
	}
	if _, e = tx.Exec(ctx, "DELETE FROM campaign_rows WHERE campaign_id=$1", id); e != nil {
		return e
	}
	seen := map[string]bool{}
	for n, row := range rows[1:] {
		recipient := cell(row, emailColumn)
		vars := map[string]string{}
		for _, name := range required {
			vars[name] = constants[name]
			if column, ok := columns[name]; ok && column >= 0 {
				vars[name] = cell(row, column)
			}
		}
		errs := []string{}
		if !validAddress(recipient) {
			errs = append(errs, "Invalid recipient email")
		}
		if _, err := renderMessageWith(ctx, tx, scope, templateID, version, vars); err != nil {
			errs = append(errs, err.Error())
		}
		encoded, _ := json.Marshal(vars)
		dispatch := tokenHash(strings.ToLower(recipient) + "|" + templateID + "|" + strconv.Itoa(version) + "|" + string(encoded))
		if seen[dispatch] {
			errs = append(errs, "Duplicate recipient message")
		}
		seen[dispatch] = true
		_, e = tx.Exec(ctx, "INSERT INTO campaign_rows(id,campaign_id,ordinal,recipient,template_id,template_version,variables,errors,dispatch_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", newID("row_"), id, n+2, recipient, templateID, version, encoded, errs, dispatch)
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
