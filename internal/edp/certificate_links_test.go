package edp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCertificateFilenameEvidence(t *testing.T) {
	rows := [][]string{{"name", "id", "link"}, {"Alice Example", "CERT-17", ""}, {"Bob Example", "CERT-18", ""}, {"Alice Example", "CERT-17", ""}, {"Unrelated", "NO-99", ""}, {"Existing", "CERT-20", "https://example.test/already"}, {"ณัฐพล ทดสอบ", "CERT-21", ""}}
	files := []drivePDF{{ID: "alice", Name: "CERT-17_Alice Example.pdf"}, {ID: "bob1", Name: "CERT-18_Bob Example.pdf"}, {ID: "bob2", Name: "CERT-18_Bob Example copy.pdf"}, {ID: "thai", Name: "CERT-21_ณัฐพล ทดสอบ.pdf"}}
	matches := matchCertificates(rows, files, []int{0, 1}, 2)
	if matches[0].Score != 100 || !matches[0].Ambiguous || !matches[2].Ambiguous {
		t.Fatal("shared PDF suggestion must require review", matches)
	}
	if !matches[1].Ambiguous {
		t.Fatal("tied filenames must require review", matches[1])
	}
	if matches[3].Score >= 90 || matches[4].Status != "existing" || matches[5].Score != 100 || matches[5].Ambiguous {
		t.Fatal("identity scores or existing links", matches)
	}
	missing := matchCertificates([][]string{{"name", "id", "link"}, {"Alice Example", "", ""}}, files, []int{0, 1}, 2)
	if missing[0].Score >= 90 {
		t.Fatal("missing identity treated as strong match", missing)
	}
	if fuzzySimilarity("Alice Example", "Alic Example") >= 90 {
		t.Fatal("fuzzy evidence scored as exact")
	}
	for _, link := range []string{"http://drive.google.com/drive/folders/id", "https://evil.test/drive/folders/id", "https://drive.google.com/drive/folders/a'b", "https://drive.google.com/file/d/id/view", "https://user:pass@drive.google.com/drive/folders/id"} {
		if _, err := driveFolderID(link); err == nil {
			t.Fatal("unsafe folder accepted", link)
		}
	}
	if id, err := driveFolderID("https://drive.google.com/drive/u/0/folders/Folder_1?usp=sharing"); err != nil || id != "Folder_1" {
		t.Fatal(id, err)
	}
}

type driveRoundTrip func(*http.Request) (*http.Response, error)

func (f driveRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDriveListingPaginationAndLimits(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: driveRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if r.URL.Host != "www.googleapis.com" || !strings.Contains(q.Get("q"), "'folder' in parents") || q.Get("supportsAllDrives") != "true" {
			t.Fatal("unexpected Drive request", r.URL)
		}
		body := `{"files":[{"id":"first","name":"One.pdf"}],"nextPageToken":"next"}`
		if calls == 2 {
			if q.Get("pageToken") != "next" {
				t.Fatal("page token missing")
			}
			body = `{"files":[{"id":"second","name":"Two.pdf"}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	files, err := listDrivePDFs(context.Background(), client, "folder")
	if err != nil || len(files) != 2 || calls != 2 {
		t.Fatal(files, err, calls)
	}
	client.Transport = driveRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"incompleteSearch":true}`))}, nil
	})
	if _, err = listDrivePDFs(context.Background(), client, "folder"); err == nil {
		t.Fatal("incomplete search accepted")
	}
}
func workspaceRequest(t *testing.T, a *App, cookie *http.Cookie, id string, input any) *httptest.ResponseRecorder {
	t.Helper()
	method := "GET"
	var body io.Reader
	if input != nil {
		method = "POST"
		raw, _ := json.Marshal(input)
		body = strings.NewReader(string(raw))
	}
	r := httptest.NewRequest(method, "/campaigns/"+id+"/workspace", body)
	r.AddCookie(cookie)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
		var csrf string
		if err := a.DB.QueryRow(context.Background(), "SELECT csrf FROM sessions WHERE token_hash=$1", tokenHash(cookie.Value)).Scan(&csrf); err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func TestCertificateLinkReviewAndDraftEdits(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	key := automationKey(t, a, "app_demo", []string{"campaign.edit", "campaign.view", "template.edit"}, root)
	tpl := automationObject(t, automationRequest(t, a, key, "POST", "/v1/templates", TemplateInput{Slug: "synthetic.links", Name: "Links", Subject: "Certificate", HTML: `<p>{{school_name}}</p><a href="{{certificate_url}}">PDF</a>`}), 201)["id"].(string)
	cid := automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns", map[string]any{"name": "Link review", "kind": "certificate"}), 201)["id"].(string)
	rows := [][]string{{"email", "school", "name", "url"}, {"alice@example.test", "Example School", "Alice Example", ""}, {"bob@example.test", "Example School", "Bob Example", ""}}
	if err := a.replaceCampaignSource(ctx, cid, rows); err != nil {
		t.Fatal(err)
	}
	files := []drivePDF{{ID: "alice", Name: "Alice Example.pdf"}, {ID: "bob", Name: "Bob Example.pdf"}}
	links := certificateLinks{Folder: "https://drive.google.com/drive/folders/example", URLColumn: 3, Columns: []int{2}, Files: files, Matches: matchCertificates(rows, files, []int{2}, 3)}
	raw, _ := json.Marshal(links)
	if _, err := a.DB.Exec(ctx, "UPDATE campaigns SET certificate_links=$2 WHERE id=$1", cid, raw); err != nil {
		t.Fatal(err)
	}
	mapping := Mapping{Email: 0, School: 1, URL: 3, Event: -1, Award: -1, DefaultAward: "participant", Rules: []AwardRule{{Award: "participant", Template: tpl}}}
	if err := a.mapCampaign(ctx, cid, mapping); err != nil {
		t.Fatal(err)
	}
	cookie := runtimeTestSession(t, a, true)
	request := map[string]any{"action": "auto", "threshold": 95, "revision": workspaceRevision(rows, links)}
	out := automationObject(t, workspaceRequest(t, a, cookie, cid, request), 200)
	if err := a.approveCampaign(ctx, cid, root); err != nil {
		t.Fatal("accepted links did not permit approval", err)
	}
	if w := workspaceRequest(t, a, cookie, cid, map[string]any{"action": "edit", "rows": rows, "revision": out["revision"]}); w.Code != 409 {
		t.Fatal("approved source changed", w.Code, w.Body.String())
	}
	if _, err := a.DB.Exec(ctx, "UPDATE campaigns SET state='draft' WHERE id=$1", cid); err != nil {
		t.Fatal(err)
	}
	if w := workspaceRequest(t, a, cookie, cid, request); w.Code != 409 {
		t.Fatal("stale review overwrite accepted", w.Code)
	}
	// Mark one source excluded and edit the name: generated links must be cleared,
	// exclusions retained, and approval must fail until review is completed again.
	a.DB.Exec(ctx, "UPDATE campaign_rows SET excluded=true WHERE campaign_id=$1 AND ordinal=3", cid)
	b, _ := json.Marshal(out["rows"])
	json.Unmarshal(b, &rows)
	rows[1][2] = "Different Person"
	edit := automationObject(t, workspaceRequest(t, a, cookie, cid, map[string]any{"action": "edit", "rows": rows, "revision": out["revision"]}), 200)
	b, _ = json.Marshal(edit["rows"])
	json.Unmarshal(b, &rows)
	if rows[1][3] != "" || rows[2][3] != "" {
		t.Fatal("stale URLs retained after edit", rows)
	}
	var excluded bool
	a.DB.QueryRow(ctx, "SELECT excluded FROM campaign_rows WHERE campaign_id=$1 AND ordinal=3", cid).Scan(&excluded)
	if !excluded {
		t.Fatal("editing lost an exclusion")
	}
	if err := a.approveCampaign(ctx, cid, root); err == nil {
		t.Fatal("unreviewed changed identity approved")
	}
	// A pending decision cannot be bypassed by supplying an unrelated HTTPS URL.
	rows[1][3] = "https://example.test/manual"
	raw, _ = json.Marshal(rows)
	links.Matches[0].Status = "pending"
	rawLinks, _ := json.Marshal(links)
	a.DB.Exec(ctx, "UPDATE campaigns SET source_rows=$2,certificate_links=$3 WHERE id=$1", cid, raw, rawLinks)
	if err := a.mapCampaign(ctx, cid, mapping); err != nil {
		t.Fatal(err)
	}
	if err := a.approveCampaign(ctx, cid, root); err == nil {
		t.Fatal("pending review bypassed by URL")
	}
	badMapping := mapping
	badMapping.URL = 2
	if err := a.mapCampaign(ctx, cid, badMapping); err == nil {
		t.Fatal("reviewed column bypassed")
	}
}
func TestFlexibleDelimitedImports(t *testing.T) {
	for _, sep := range []string{";", "\t", ","} {
		raw := "email" + sep + "name\nuser@example.test" + sep + "Example\n"
		rows, err := parseImport(strings.NewReader(raw), "recipients.tsv", "")
		if err != nil || !reflect.DeepEqual(rows, [][]string{{"email", "name"}, {"user@example.test", "Example"}}) {
			t.Fatal(sep, rows, err)
		}
	}
	punctuation, err := parseCSV(strings.NewReader("email,name\nuser@example.test,Alice;Bob;Charlie\n"))
	if err != nil || len(punctuation[0]) != 2 || punctuation[1][1] != "Alice;Bob;Charlie" {
		t.Fatal("cell punctuation replaced the actual CSV delimiter", punctuation, err)
	}
	withTitle, err := parseCSV(strings.NewReader("Recipient list\nemail;name\nuser@example.test;Example\n"))
	if err != nil || len(withTitle[1]) != 2 {
		t.Fatal("title row hid delimiter", withTitle, err)
	}
	rows, err := importHeader([][]string{{"Title"}, {"email", "name"}, {"user@example.test", "Example"}}, "2")
	if err != nil || rows[0][0] != "email" {
		t.Fatal(rows, err)
	}
}
