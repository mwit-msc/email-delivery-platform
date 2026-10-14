package edp

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
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
	// Prefer a consistent record width, so punctuation inside a data cell
	// cannot outweigh the delimiter used by the header and surrounding rows.
	best := 0
	for _, delimiter := range []rune{',', '\t', ';'} {
		probe := csv.NewReader(strings.NewReader(string(data)))
		probe.Comma, probe.FieldsPerRecord = delimiter, -1
		probe.TrimLeadingSpace = true
		widths := map[int]int{}
		for record := 0; record < 10; record++ {
			fields, err := probe.Read()
			if err != nil {
				break
			}
			if len(fields) > 1 {
				widths[len(fields)]++
			}
		}
		for width, count := range widths {
			if score := count*100 + min(width, 52); score > best {
				best, r.Comma = score, delimiter
			}
		}
	}
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
	return a.sheetRowsForUser(ctx, link, "")
}
func (a *App) sheetRowsForUser(ctx context.Context, link, user string) ([][]string, error) {
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
	owner, connection, e := a.googleReadIdentity(ctx, user)
	if e != nil {
		return nil, e
	}
	client, e := a.googleReadClientScope(ctx, owner, connection, "https://www.googleapis.com/auth/spreadsheets.readonly")
	if e != nil {
		return nil, e
	}

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
			return fmt.Errorf("Google Sheets returned %d; check read access for the connected Google account", res.StatusCode)
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
	Email                int               `json:"email"`
	School               int               `json:"school"`
	Event                int               `json:"event"`
	Award                int               `json:"award"`
	URL                  int               `json:"url"`
	DefaultEvent         string            `json:"default_event"`
	DefaultAward         string            `json:"default_award"`
	Rules                []AwardRule       `json:"rules"`
	Columns              map[string]int    `json:"columns"`
	Constants            map[string]string `json:"constants"`
	SkipRowsWithoutEmail bool              `json:"skip_rows_without_email"`
	CombineRecipients    bool              `json:"combine_recipients"`
	CombinedTemplate     string            `json:"combined_template"`
	NameColumns          []int             `json:"name_columns"`
}

type AwardRule struct {
	Event     string            `json:"event"`
	Award     string            `json:"award"`
	Template  string            `json:"template"`
	Variables map[string]string `json:"variables,omitempty"`
}

var variableName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,99}$`)

func selectedAwardRule(rules []AwardRule, event, award string) *AwardRule {
	var generic *AwardRule
	for i := range rules {
		rule := &rules[i]
		if rule.Award != award {
			continue
		}
		if rule.Event == event {
			return rule
		}
		if rule.Event == "" {
			generic = rule
		}
	}
	return generic
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
	if e = a.mapCampaignWith(ctx, tx, id, m); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (a *App) mapCampaignWith(ctx context.Context, tx pgx.Tx, id string, m Mapping) error {
	if m.CombineRecipients && (m.CombinedTemplate == "" || len(m.NameColumns) == 0) {
		return fmt.Errorf("combining certificates requires a combined template and participant name columns")
	}
	if len(m.NameColumns) > 52 {
		return fmt.Errorf("at most 52 participant name columns")
	}
	for _, column := range m.NameColumns {
		if column < 0 || column > 51 {
			return fmt.Errorf("invalid participant name column")
		}
	}
	if len(m.Rules) > 1000 {
		return fmt.Errorf("at most 1000 award mapping rules")
	}
	seenRules := map[string]bool{}
	for _, rule := range m.Rules {
		key := rule.Event + "\x00" + rule.Award
		if rule.Award == "" || rule.Template == "" || seenRules[key] {
			return fmt.Errorf("award rules require a unique event/award and template")
		}
		seenRules[key] = true
		if len(rule.Variables) > 64 {
			return fmt.Errorf("at most 64 variables per award rule")
		}
		for key, value := range rule.Variables {
			if !variableName.MatchString(key) || len(value) > 4096 {
				return fmt.Errorf("invalid award variable name or value size")
			}
		}
	}
	var app, state string
	var data []byte
	e := tx.QueryRow(ctx, "SELECT application_id,state,source_rows FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&app, &state, &data)
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
	var linkRaw []byte
	if e = tx.QueryRow(ctx, "SELECT certificate_links FROM campaigns WHERE id=$1", id).Scan(&linkRaw); e != nil {
		return e
	}
	links, e := decodeLinks(linkRaw)
	if e != nil {
		return e
	}
	if len(links.Matches) > 0 && (m.URL != links.URLColumn || m.Constants["certificate_url"] != "") {
		return fmt.Errorf("map certificate links to the reviewed link column; use the sheet to change links")
	}
	if len(links.Matches) > 0 {
		if _, ok := m.Columns["certificate_url"]; ok {
			return fmt.Errorf("certificate_url uses the reviewed link column")
		}
		for _, rule := range m.Rules {
			if _, ok := rule.Variables["certificate_url"]; ok {
				return fmt.Errorf("award rules cannot override reviewed certificate links")
			}
		}
	}
	linkErrors, e := campaignLinkErrors(ctx, tx, id, rows)
	if e != nil {
		return e
	}
	// A mapping change must not silently include previously excluded certificates.
	excluded := map[int]bool{}
	prior, e := tx.Query(ctx, "SELECT ordinal,source_ordinals FROM campaign_rows WHERE campaign_id=$1 AND excluded", id)
	if e != nil {
		return e
	}
	for prior.Next() {
		var ordinal int
		var sources []int
		if e = prior.Scan(&ordinal, &sources); e != nil {
			prior.Close()
			return e
		}
		if len(sources) == 0 {
			sources = []int{ordinal}
		}
		for _, source := range sources {
			excluded[source] = true
		}
	}
	e = prior.Err()
	prior.Close()
	if e != nil {
		return e
	}
	var combinedID string
	var combinedVersion int
	if m.CombineRecipients {
		var content, subject, plain string
		e = tx.QueryRow(ctx, `SELECT t.id,t.current_version,v.html,v.subject,v.plain FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE t.application_id=$1 AND (t.slug=$2 OR t.id=$2)`, app, m.CombinedTemplate).Scan(&combinedID, &combinedVersion, &content, &subject, &plain)
		if e != nil || !hasCertificateList(content) {
			return fmt.Errorf("combined template must belong to this application and contain {{certificate_list}} in its HTML")
		}
		if !hasCertificateList(plain) {
			return fmt.Errorf("combined template plain text must also contain {{certificate_list}}")
		}
		for _, match := range placeholders.FindAllStringSubmatch(content+subject+plain, -1) {
			if match[1] == "certificate_url" || match[1] == "award_text" {
				return fmt.Errorf("combined templates must use {{certificate_list}} instead of single-certificate URL or award placeholders")
			}
		}
	}
	if _, e = tx.Exec(ctx, "DELETE FROM campaign_rows WHERE campaign_id=$1", id); e != nil {
		return e
	}
	seen := map[string]bool{}
	var mapped []mappedCertificateRow
	for i, row := range rows[1:] {
		email, school, event, award, link := cell(row, m.Email), cell(row, m.School), cell(row, m.Event), cell(row, m.Award), cell(row, m.URL)
		if m.SkipRowsWithoutEmail && email == "" {
			continue
		}
		if event == "" {
			event = m.DefaultEvent
		}
		if award == "" {
			award = m.DefaultAward
		}
		var errs []string
		if message := linkErrors[i+2]; message != "" {
			errs = append(errs, message)
		}
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
		slug := event + "." + award
		rule := selectedAwardRule(m.Rules, event, award)
		if rule != nil {
			slug = rule.Template
		}
		if m.CombineRecipients {
			tid, v, err = combinedID, combinedVersion, nil
		} else {
			err = tx.QueryRow(ctx, "SELECT id,current_version FROM templates WHERE application_id=$1 AND (slug=$2 OR id=$2)", app, slug).Scan(&tid, &v)
		}
		if err != nil {
			errs = append(errs, "No template for event/award: "+event+"/"+award)
		} else {
			tpl = &tid
			version = &v
		}
		vars := map[string]string{"school_name": school, "certificate_url": link}
		for k, v := range m.Constants {
			vars[k] = v
		}
		for k, column := range m.Columns {
			if column >= 0 {
				vars[k] = cell(row, column)
			}
		}
		if rule != nil {
			for key, value := range rule.Variables {
				vars[key] = value
			}
		}
		if m.CombineRecipients {
			var names []string
			for _, column := range m.NameColumns {
				if name := cell(row, column); name != "" {
					names = append(names, name)
				}
			}
			wording := vars["award_text"]
			if wording == "" {
				wording = award
			}
			items, _ := json.Marshal([]CertificateItem{{Name: strings.Join(names, " / "), Award: wording, URL: link, SourceRow: i + 2}})
			vars["certificate_items"], vars["certificate_count"] = string(items), "1"
		}
		if tpl != nil {
			if _, err := renderMessageWith(ctx, tx, app, tid, v, vars); err != nil {
				errs = append(errs, err.Error())
			}
		}
		dispatch := tokenHash(strings.ToLower(email) + "|" + school + "|" + event + "|" + award + "|" + link)
		if seen[dispatch] {
			errs = append(errs, "Duplicate certificate dispatch")
		}
		seen[dispatch] = true
		if errs == nil {
			errs = []string{}
		}
		groupKey, _ := json.Marshal([]string{certificateRecipientKey(email), school, event})
		mapped = append(mapped, mappedCertificateRow{Ordinal: i + 2, Recipient: email, Template: tpl, Version: version, Variables: vars, Errors: errs, Dispatch: dispatch, Sources: []int{i + 2}, Excluded: excluded[i+2], GroupKey: string(groupKey)})
	}
	if m.CombineRecipients {
		if mapped, e = combineCertificateRows(mapped); e != nil {
			return e
		}
		for i := range mapped {
			row := &mapped[i]
			if len(row.Errors) == 0 {
				if _, err := renderMessageWith(ctx, tx, app, combinedID, combinedVersion, row.Variables); err != nil {
					row.Errors = append(row.Errors, err.Error())
				}
			}
		}
	}
	for _, row := range mapped {
		data, _ := json.Marshal(row.Variables)
		if _, e = tx.Exec(ctx, "INSERT INTO campaign_rows(id,campaign_id,ordinal,recipient,template_id,template_version,variables,errors,dispatch_key,source_ordinals,excluded) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", newID("row_"), id, row.Ordinal, row.Recipient, row.Template, row.Version, data, row.Errors, row.Dispatch, row.Sources, row.Excluded); e != nil {
			return e
		}
	}
	mapping, _ := json.Marshal(m)
	if _, e = tx.Exec(ctx, "UPDATE campaigns SET mapping=$2 WHERE id=$1", id, mapping); e != nil {
		return e
	}
	return nil
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
	if e = requireReviewedLinks(ctx, tx, id); e != nil {
		return e
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
	var campaignCC, campaignBCC []string
	e = tx.QueryRow(ctx, "SELECT application_id,state,cc,bcc FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&app, &state, &campaignCC, &campaignBCC)
	if e != nil {
		return e
	}
	if state != "approved" {
		return fmt.Errorf("approve campaign before scheduling")
	}
	var from, reply, fromName string
	var daily, queueLimit, recipientLimit int
	var paused bool
	e = tx.QueryRow(ctx, "SELECT from_address,reply_to,from_name,daily_limit,queue_limit,recipient_hour_limit,paused FROM applications WHERE id=$1 FOR UPDATE", app).Scan(&from, &reply, &fromName, &daily, &queueLimit, &recipientLimit, &paused)
	if e != nil {
		return e
	}
	var platformPaused bool
	if e = tx.QueryRow(ctx, "SELECT paused FROM platform_settings WHERE id=1").Scan(&platformPaused); e != nil {
		return e
	}
	if paused || platformPaused {
		return fmt.Errorf("sending is paused")
	}
	var count, used, queued int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM campaign_rows WHERE campaign_id=$1 AND NOT excluded", id).Scan(&count); e != nil {
		return e
	}
	if e = tx.QueryRow(ctx, "SELECT coalesce(sum(1+cardinality(cc)+cardinality(bcc)) FILTER(WHERE created_at>=now()-interval '24 hours'),0),count(*) FILTER(WHERE status IN('queued','retry_wait','sending')) FROM messages WHERE application_id=$1", app).Scan(&used, &queued); e != nil {
		return e
	}
	if count+queued > queueLimit*8/10 {
		return fmt.Errorf("campaign exceeds application quota or queue capacity; transactional capacity is reserved")
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
		CC, BCC           []string
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
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	additions := map[string]int{}
	deliveries := 0
	for index := range items {
		i := &items[index]
		if i.CC, i.BCC, e = normalizeCopies(i.To, campaignCC, campaignBCC); e != nil {
			return e
		}
		deliveries += 1 + len(i.CC) + len(i.BCC)
		additions[strings.ToLower(i.To)]++
		for _, address := range append(append([]string{}, i.CC...), i.BCC...) {
			additions[strings.ToLower(address)]++
		}
	}
	if deliveries+used > daily*9/10 {
		return fmt.Errorf("campaign exceeds daily recipient quota (including CC and BCC); transactional capacity is reserved")
	}
	if e = checkRecipientCapacity(ctx, tx, app, additions, recipientLimit); e != nil {
		return e
	}
	relayID, relayError := senderRelayWith(ctx, tx, from)
	if relayError != nil {
		return relayError
	}
	for _, i := range items {
		rendered, e := renderMessageWith(ctx, tx, app, i.Template, i.Version, i.Vars)
		if e != nil {
			return e
		}
		mid := newID("msg_")
		_, e = tx.Exec(ctx, `INSERT INTO messages(id,application_id,campaign_id,recipient,from_address,reply_to,subject,html,plain,class,idempotency_key,payload_hash,available_at,expires_at,relay_id,cc,from_name,bcc) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'bulk',$10,$11,$12,$13,NULLIF($14,''),$15,$16,$17)`, mid, app, id, i.To, from, reply, rendered.Subject, rendered.HTML, rendered.Plain, "campaign:"+id+":"+i.Key, i.Key, scheduled, scheduled.Add(7*24*time.Hour), relayID, i.CC, fromName, i.BCC)
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
		case "bcc":
			e = a.setCampaignBCC(r.Context(), id, parseCC(r.FormValue("bcc")))
		case "cc":
			e = a.setCampaignCC(r.Context(), id, parseCC(r.FormValue("cc")))
		case "import":
			if data["state"] != "draft" {
				e = fmt.Errorf("return campaign to draft before importing")
				break
			}
			var rows [][]string
			if r.FormValue("sheet_url") != "" {
				rows, e = a.sheetRowsForUser(r.Context(), r.FormValue("sheet_url"), person(r).ID)
			} else if text := r.FormValue("paste"); strings.TrimSpace(text) != "" {
				rows, e = parseCSV(strings.NewReader(text))
			} else {
				file, header, err := r.FormFile("csv")
				if err != nil {
					e = fmt.Errorf("choose a CSV or Excel file or provide a sheet URL")
					break
				}
				defer file.Close()
				rows, e = parseImport(file, header.Filename, r.FormValue("sheet"))
			}
			if e == nil {
				rows, e = importHeader(rows, r.FormValue("header_row"))
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
				m := Mapping{Email: parse("email"), School: parse("school"), Event: parse("event"), Award: parse("award"), URL: parse("url"), DefaultEvent: r.FormValue("default_event"), DefaultAward: r.FormValue("default_award"), SkipRowsWithoutEmail: r.FormValue("skip_empty_email") == "on", CombineRecipients: r.FormValue("combine_recipients") == "on", CombinedTemplate: r.FormValue("combined_template")}
				for _, value := range r.Form["name_columns"] {
					column, err := strconv.Atoi(value)
					if err != nil {
						problem(w, 400, "invalid participant name column")
						return
					}
					m.NameColumns = append(m.NameColumns, column)
				}
				// Certificate forms do not expose API column/constant mappings.
				// Retain these values when an organizer changes an award selection.
				var previous Mapping
				stored, _ := json.Marshal(data["mapping"])
				if json.Unmarshal(stored, &previous) == nil {
					m.Columns, m.Constants = previous.Columns, previous.Constants
				}
				if m.Columns == nil {
					m.Columns = map[string]int{}
				}
				if m.Constants == nil {
					m.Constants = map[string]string{}
				}
				for key := range r.Form {
					if strings.HasPrefix(key, "var.") {
						m.Columns[strings.TrimPrefix(key, "var.")] = parse(key)
					}
					if strings.HasPrefix(key, "const.") {
						m.Constants[strings.TrimPrefix(key, "const.")] = r.FormValue(key)
					}
					if strings.HasPrefix(key, "rule.") && (r.FormValue(key) != "" || m.CombineRecipients) {
						award := strings.TrimPrefix(key, "rule.")
						rule := AwardRule{Award: award, Template: r.FormValue(key)}
						if rule.Template == "" {
							rule.Template = m.CombinedTemplate
						}
						if raw := strings.TrimSpace(r.FormValue("rule_vars." + award)); raw != "" {
							if err := json.Unmarshal([]byte(raw), &rule.Variables); err != nil {
								problem(w, 400, "award variables must be a JSON object of text values")
								return
							}
						}
						m.Rules = append(m.Rules, rule)
					}
				}
				e = a.mapCampaign(r.Context(), id, m)
			}
		case "exclude":
			e = a.toggleCampaignRow(r.Context(), id, r.FormValue("row"))

		case "approve", "draft", "start", "pause", "resume", "cancel", "test":
			in := CampaignAction{To: r.FormValue("test_to")}
			if value := r.FormValue("schedule"); value != "" {
				var when time.Time
				when, e = time.ParseInLocation("2006-01-02T15:04", value, time.FixedZone("Asia/Bangkok", 7*3600))
				in.ScheduleAt = &when
			}
			if e == nil {
				e = a.actCampaign(r.Context(), id, scope, person(r).ID, action, in)
			}
		default:
			e = fmt.Errorf("unknown campaign action")
		}
		if e != nil {
			problem(w, 400, e.Error())
			return
		}
		a.audit(r.Context(), person(r).ID, "campaign."+action, scope, id)
		target := "/campaigns/" + id
		if r.FormValue("return_to") == "sheet" {
			target += "/sheet?tab=mapping"
		}
		http.Redirect(w, r, target, 303)
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
	templates, _ := a.query(r.Context(), "SELECT t.id,t.slug,t.name,v.variables FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=t.current_version WHERE application_id=$1 AND NOT t.archived ORDER BY name", scope)
	selected := r.URL.Query().Get("template")
	if selected == "" && data["kind"] == "bulk" {
		var saved BulkMapping
		raw, _ := json.Marshal(data["mapping"])
		json.Unmarshal(raw, &saved)
		selected = saved.Template
	}
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
	mapping := Mapping{Email: -1, School: -1, Event: -1, Award: -1, URL: -1}
	rawMapping, _ := json.Marshal(data["mapping"])
	json.Unmarshal(rawMapping, &mapping)
	nameColumns := map[int]bool{}
	for _, column := range mapping.NameColumns {
		nameColumns[column] = true
	}
	certificates := 0
	for _, row := range rows {
		var sources []int
		raw, _ := json.Marshal(row["source_ordinals"])
		json.Unmarshal(raw, &sources)
		if len(sources) == 0 {
			sources = []int{int(row["ordinal"].(int32))}
		}
		labels := make([]string, len(sources))
		for i, source := range sources {
			labels[i] = strconv.Itoa(source)
		}
		row["source_label"], row["certificate_count"] = strings.Join(labels, ", "), len(sources)
		certificates += len(sources)
	}
	if mapping.Email < 0 {
		for i, h := range headers {
			lower := strings.ToLower(h)
			switch {
			case strings.Contains(lower, "email") || strings.Contains(h, "อีเมล"):
				mapping.Email = i
			case strings.Contains(lower, "school") || h == "โรงเรียน":
				mapping.School = i
			case strings.Contains(lower, "certificate") || strings.Contains(lower, "url"):
				mapping.URL = i
			case strings.Contains(lower, "award") || h == "ประเภทเกียรติบัตร":
				mapping.Award = i
			case lower == "event":
				mapping.Event = i
			}
		}
	}
	ruleSelections := map[string]string{}
	ruleVariables := map[string]string{}
	for _, rule := range mapping.Rules {
		ruleSelections[rule.Award] = rule.Template
		if len(rule.Variables) > 0 {
			raw, _ := json.Marshal(rule.Variables)
			ruleVariables[rule.Award] = string(raw)
		}
	}
	certificateVariables := []string{}
	seenVariables := map[string]bool{"school_name": true, "certificate_url": true, "certificate_items": true, "certificate_count": true, "certificate_list": true}
	for _, t := range templates {
		raw, _ := json.Marshal(t["variables"])
		var names []string
		json.Unmarshal(raw, &names)
		for _, name := range names {
			if !seenVariables[name] {
				seenVariables[name] = true
				certificateVariables = append(certificateVariables, name)
			}
		}
	}
	sort.Strings(certificateVariables)
	fields, mappingJSON := campaignMappingReview(data["kind"].(string), data["mapping"], headers)
	page := "campaign"
	if strings.HasSuffix(r.URL.Path, "/sheet") {
		page = "campaign-sheet-editor"
	}
	googleView := a.googleConnectionView(r.Context(), person(r).ID)
	googleReturn := r.URL.RequestURI()
	if page == "campaign-sheet-editor" {
		googleReturn = "/campaigns/" + id + "/sheet?tab=review"
	}
	a.render(w, r, page, map[string]any{"CertificateVariables": certificateVariables, "MappingFields": fields, "MappingJSON": mappingJSON, "Campaign": data, "Rows": rows, "Headers": headers, "Awards": sourceAwards(source), "Mapping": mapping, "NameColumns": nameColumns, "CertificateCount": certificates, "RuleSelections": ruleSelections, "RuleVariables": ruleVariables, "Scope": scope, "SheetConfigured": googleView["CanRead"], "Google": googleView, "GoogleReturnTo": googleReturn, "GoogleResult": r.URL.Query().Get("google"), "Templates": templates, "SelectedTemplate": selected, "TemplateVariables": variables})
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
	config, e := a.runtimeConfig(r.Context())
	if e != nil {
		problem(w, 503, "preview configuration unavailable")
		return
	}
	w.Header().Set("Content-Security-Policy", emailPreviewCSP())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(emailDocument(rendered.HTML, config.PublicURL)))
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
	var validation error
	rows, validation = normalizeImportRows(rows)
	if validation != nil {
		return validation
	}
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
	if _, e = tx.Exec(ctx, "DELETE FROM certificate_scan_files WHERE campaign_id=$1", id); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE campaigns SET source_rows=$2,mapping='{}',certificate_links='{}' WHERE id=$1", id, raw); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM campaign_rows WHERE campaign_id=$1", id); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) toggleCampaignRow(ctx context.Context, id, row string) error {
	return a.setCampaignRowExcluded(ctx, id, row, nil)
}

func (a *App) setCampaignRowExcluded(ctx context.Context, id, row string, excluded *bool) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var state, kind string
	var raw []byte
	if e = tx.QueryRow(ctx, "SELECT state,kind,mapping FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&state, &kind, &raw); e != nil {
		return e
	}
	if state != "draft" {
		return fmt.Errorf("only draft rows may change")
	}
	var current bool
	if e = tx.QueryRow(ctx, "SELECT excluded FROM campaign_rows WHERE campaign_id=$1 AND id=$2", id, row).Scan(&current); e != nil {
		return fmt.Errorf("row not found")
	}
	value := !current
	if excluded != nil {
		value = *excluded
	}
	if _, e = tx.Exec(ctx, "UPDATE campaign_rows SET excluded=$3 WHERE campaign_id=$1 AND id=$2", id, row, value); e != nil {
		return e
	}
	var mapping Mapping
	if kind == "certificate" && current != value && json.Unmarshal(raw, &mapping) == nil && mapping.CombineRecipients {
		if e = a.mapCampaignWith(ctx, tx, id, mapping); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

func (a *App) mapBulkCampaign(ctx context.Context, id, templateID string, emailColumn int, columns map[string]int, constants map[string]string) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = a.mapBulkCampaignWith(ctx, tx, id, templateID, emailColumn, columns, constants); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) mapBulkCampaignWith(ctx context.Context, tx pgx.Tx, id, templateID string, emailColumn int, columns map[string]int, constants map[string]string) error {
	var e error
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
	excluded := map[int]bool{}
	prior, e := tx.Query(ctx, "SELECT ordinal FROM campaign_rows WHERE campaign_id=$1 AND excluded", id)
	if e != nil {
		return e
	}
	for prior.Next() {
		var ordinal int
		if e = prior.Scan(&ordinal); e != nil {
			prior.Close()
			return e
		}
		excluded[ordinal] = true
	}
	e = prior.Err()
	prior.Close()
	if e != nil {
		return e
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
		_, e = tx.Exec(ctx, "INSERT INTO campaign_rows(id,campaign_id,ordinal,recipient,template_id,template_version,variables,errors,dispatch_key,excluded) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", newID("row_"), id, n+2, recipient, templateID, version, encoded, errs, dispatch, excluded[n+2])
		if e != nil {
			return e
		}
	}
	mapping, e := json.Marshal(BulkMapping{Template: templateID, Email: emailColumn, Columns: columns, Constants: constants})
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE campaigns SET mapping=$2 WHERE id=$1", id, mapping); e != nil {
		return e
	}
	return nil
}

func sourceAwards(rows [][]string) []string {
	if len(rows) == 0 {
		return nil
	}
	column := -1
	for i, h := range rows[0] {
		if strings.Contains(strings.ToLower(h), "award") || h == "ประเภทเกียรติบัตร" {
			column = i
			break
		}
	}
	var out []string
	for _, r := range rows[1:] {
		if v := cell(r, column); v != "" && !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
