package edp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCertificateListEscapingAndValidation(t *testing.T) {
	legacy, err := renderVariables("{{certificate_list}}", map[string]string{"certificate_list": "Legacy <text>"}, true)
	if err != nil || legacy != "Legacy &lt;text&gt;" {
		t.Fatal("legacy scalar variable behavior changed", legacy, err)
	}
	items, _ := json.Marshal([]CertificateItem{{Name: `<img src=x onerror=evil()>`, Award: `Winner & participant`, URL: `https://example.test/a?x=1&y=2`}, {Name: "Second", Award: "Teacher", URL: "https://example.test/b"}})
	vars := map[string]string{"certificate_items": string(items), "certificate_list": "<script>evil</script>", "certificate_link_text": "Open <file>"}
	rendered, err := renderTemplateInput(TemplateInput{Subject: "Certificates", HTML: `<div>{{certificate_list}}</div>`, Plain: "{{certificate_list}}"}, vars)
	if err != nil || strings.Contains(rendered.HTML, "<img") || strings.Contains(rendered.HTML, "<script") || !strings.Contains(rendered.HTML, "&lt;img") || !strings.Contains(rendered.HTML, "Open &lt;file&gt;") || !strings.Contains(rendered.Plain, "https://example.test/b") {
		t.Fatal("unsafe or incomplete certificate list", err, rendered)
	}
	for _, raw := range []string{"[]", `[{"name":"Person","award":"Winner","url":"javascript:evil()"}]`, `[{"name":"Person","award":"Winner","url":"https://user:pass@example.test"}]`, `[{"name":"Person","award":"Winner","url":"https://example.test","html":"evil"}]`, string(items) + " {}", `[{"name":"","award":"Winner","url":"https://example.test"}]`} {
		if _, err := certificateList(raw, "", true); err == nil {
			t.Fatal("invalid list accepted", raw)
		}
	}
	_, required, err := prepareTemplate(TemplateInput{Subject: "Certificates", HTML: "{{certificate_list}}"})
	if err != nil || len(required) != 1 || required[0] != "certificate_items" {
		t.Fatal("list requires structured data rather than raw HTML", required, err)
	}
}

func TestCertificateGroupingPreservesItemsAndApproval(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	key := automationKey(t, a, "app_demo", []string{"template.view", "template.edit", "campaign.view", "campaign.edit", "campaign.approve", "campaign.send"}, root)
	base := TemplateInput{Slug: "synthetic.single", Name: "Single", Subject: "Certificate", HTML: `<p>{{school_name}} {{award_text}}</p><a href="{{certificate_url}}">File</a>`}
	single := automationObject(t, automationRequest(t, a, key, "POST", "/v1/templates", base), 201)["id"].(string)
	digest := TemplateInput{Slug: "synthetic.combined", Name: "Combined", Subject: "{{certificate_count}} certificates for {{school_name}}", HTML: `<p>{{school_name}}</p>{{certificate_list}}`, Plain: "{{school_name}}\n{{certificate_list}}"}
	combined := automationObject(t, automationRequest(t, a, key, "POST", "/v1/templates", digest), 201)["id"].(string)
	cid := automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns", map[string]any{"name": "Combined", "kind": "certificate"}), 201)["id"].(string)
	rows := [][]string{{"email", "school", "event", "award", "link", "name"},
		{"contact@example.test", "School", "Event", "Winner", "https://example.test/a", "Alice"},
		{"contact@example.test", "School", "Event", "Participation", "https://example.test/b", "Bob"},
		{"contact@example.test", "Other school", "Event", "Winner", "https://example.test/c", "Charlie"},
		{"contact@example.test", "School", "Other event", "Winner", "https://example.test/d", "Dee"},
		{"other@example.test", "School", "Event", "Winner", "https://example.test/e", "Eve"},
		{"bad email", "School", "Event", "Participation", "https://example.test/f", "Frank"},
		{"contact@example.test", "School", "Event", "Winner", "https://example.test/a", "Alice"},
		{"contact@example.test", "School", "Event", "Teacher", "https://example.test/g", "Teacher"}}
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/import", map[string]any{"rows": rows}), 200)
	mapping := Mapping{Email: 0, School: 1, Event: 2, Award: 3, URL: 4, Rules: []AwardRule{{Award: "Winner", Template: single, Variables: map[string]string{"award_text": "Winner"}}, {Award: "Participation", Template: single, Variables: map[string]string{"award_text": "Participation"}}, {Award: "Teacher", Template: single, Variables: map[string]string{"award_text": "Teacher"}}}}
	if err := a.mapCampaign(ctx, cid, mapping); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, "UPDATE campaign_rows SET excluded=true WHERE campaign_id=$1 AND ordinal=9", cid); err != nil {
		t.Fatal(err)
	}
	mapping.CombineRecipients, mapping.CombinedTemplate, mapping.NameColumns = true, combined, []int{5}
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/mapping", mapping), 200)
	var entries, sources, excluded, jobs int
	a.DB.QueryRow(ctx, "SELECT count(*),sum(cardinality(source_ordinals)),count(*) FILTER(WHERE excluded) FROM campaign_rows WHERE campaign_id=$1", cid).Scan(&entries, &sources, &excluded)
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1", cid).Scan(&jobs)
	if entries != 7 || sources != 8 || excluded != 1 || jobs != 0 {
		t.Fatal("lost source certificates, exclusions or draft-only behavior", entries, sources, excluded, jobs)
	}
	var data []byte
	var ordinals []int
	a.DB.QueryRow(ctx, "SELECT variables,source_ordinals FROM campaign_rows WHERE campaign_id=$1 AND ordinal=2", cid).Scan(&data, &ordinals)
	var vars map[string]string
	json.Unmarshal(data, &vars)
	var items []CertificateItem
	json.Unmarshal([]byte(vars["certificate_items"]), &items)
	if len(items) != 2 || items[0].Name != "Alice" || items[1].URL != "https://example.test/b" || items[1].Award != "Participation" || len(ordinals) != 2 || ordinals[1] != 3 {
		t.Fatal("item identity or source lineage lost", items, ordinals)
	}
	// UI and export identify the included source rows instead of hiding grouping.
	r := httptest.NewRequest("GET", "/campaigns/"+cid, nil)
	r.AddCookie(runtimeTestSession(t, a, true))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "2 certificates in one email") || !strings.Contains(w.Body.String(), "2, 3") {
		t.Fatal("grouping UI failed", w.Code)
	}
	export := automationRequest(t, a, key, "GET", "/v1/campaigns/"+cid+"/export", nil)
	if export.Code != http.StatusOK || !strings.Contains(export.Body.String(), "source_rows,certificate_count") || !strings.Contains(export.Body.String(), "2; 3,2") {
		t.Fatal("export lost lineage", export.Code)
	}
	if err := a.approveCampaign(ctx, cid, root); err == nil {
		t.Fatal("invalid or duplicate certificates bypassed approval")
	}
	// Including a formerly excluded item must regroup atomically, not add a
	// second email to an existing recipient group.
	var teacherRow string
	a.DB.QueryRow(ctx, "SELECT id FROM campaign_rows WHERE campaign_id=$1 AND ordinal=9", cid).Scan(&teacherRow)
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/exclude", map[string]any{"row_id": teacherRow, "excluded": false}), 200)
	a.DB.QueryRow(ctx, "SELECT variables FROM campaign_rows WHERE campaign_id=$1 AND ordinal=2", cid).Scan(&data)
	json.Unmarshal(data, &vars)
	json.Unmarshal([]byte(vars["certificate_items"]), &items)
	if len(items) != 3 || items[2].Name != "Teacher" || items[2].Award != "Teacher" {
		t.Fatal("included item did not regroup with its own wording", items)
	}
	// Excluding a combined entry excludes every underlying source item.
	var groupRow string
	a.DB.QueryRow(ctx, "SELECT id FROM campaign_rows WHERE campaign_id=$1 AND ordinal=2", cid).Scan(&groupRow)
	if err := a.toggleCampaignRow(ctx, cid, groupRow); err != nil {
		t.Fatal(err)
	}
	a.DB.QueryRow(ctx, "SELECT sum(cardinality(source_ordinals)) FROM campaign_rows WHERE campaign_id=$1 AND excluded", cid).Scan(&excluded)
	if excluded != 3 {
		t.Fatal("excluding combined entry lost item exclusions", excluded)
	}
	// Restore just the two student items for the remaining approval proof.
	if _, err := a.DB.Exec(ctx, "UPDATE campaign_rows SET excluded=false WHERE campaign_id=$1 AND ordinal IN(2,3)", cid); err != nil {
		t.Fatal(err)
	}
	// The form can supply per-award wording without selecting a redundant
	// individual template while using the combined template.
	fields := url.Values{"csrf": {"runtime-csrf"}, "action": {"map"}, "email": {"0"}, "school": {"1"}, "event": {"2"}, "award": {"3"}, "url": {"4"}, "combine_recipients": {"on"}, "combined_template": {combined}, "name_columns": {"5"}, "rule.Winner": {""}, "rule.Participation": {""}, "rule.Teacher": {""}, "rule_vars.Participation": {`{"award_text":"Participant wording"}`}}
	formRequest := httptest.NewRequest("POST", "/campaigns/"+cid, strings.NewReader(fields.Encode()))
	formRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	formRequest.AddCookie(runtimeTestSession(t, a, true))
	formResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(formResponse, formRequest)
	if formResponse.Code != http.StatusSeeOther {
		t.Fatal("combined form mapping failed", formResponse.Code, formResponse.Body.String())
	}
	a.DB.QueryRow(ctx, "SELECT variables FROM campaign_rows WHERE campaign_id=$1 AND ordinal=2", cid).Scan(&data)
	json.Unmarshal(data, &vars)
	json.Unmarshal([]byte(vars["certificate_items"]), &items)
	if len(items) != 2 || items[1].Award != "Participant wording" {
		t.Fatal("form silently dropped award wording without an individual template", items)
	}
	// Turning grouping off and back on must preserve excluded individual items.
	mapping.CombineRecipients = false
	if err := a.mapCampaign(ctx, cid, mapping); err != nil {
		t.Fatal(err)
	}
	mapping.CombineRecipients = true
	if err := a.mapCampaign(ctx, cid, mapping); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, "UPDATE campaign_rows SET excluded=true WHERE campaign_id=$1 AND ordinal IN(7,8)", cid); err != nil {
		t.Fatal(err)
	}
	if err := a.approveCampaign(ctx, cid, root); err != nil {
		t.Fatal(err)
	}
	digest.Version, digest.HTML = 1, "New version"
	if _, _, err := a.saveTemplateVersion(ctx, "app_demo", combined, digest); err != nil {
		t.Fatal(err)
	}
	if err := a.mapCampaign(ctx, cid, mapping); err == nil {
		t.Fatal("approved contents changed")
	}
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/start", map[string]any{}), 200)
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1", cid).Scan(&jobs)
	var body string
	a.DB.QueryRow(ctx, "SELECT html FROM messages WHERE campaign_id=$1 AND subject LIKE '2 certificates%'", cid).Scan(&body)
	if jobs != 4 || !strings.Contains(body, "Alice") || !strings.Contains(body, "Bob") || strings.Contains(body, "New version") {
		t.Fatal("grouped delivery count or immutable rendering failed", jobs, body)
	}
}

func TestCertificateGroupingRejectsUnsafeTemplateAndMissingNames(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	cid := "cmp_invalid_grouping"
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	if _, err := a.DB.Exec(ctx, "INSERT INTO campaigns(id,application_id,name,created_by,source_rows) VALUES($1,'app_demo','Validation',$2,$3)", cid, root, []byte(`[["email","school","award","url","name"],["one@example.test","School","Winner","https://example.test/one","One"]]`)); err != nil {
		t.Fatal(err)
	}
	input := TemplateInput{Slug: "synthetic.bad_combined", Name: "Bad combined", Subject: "{{award_text}}", HTML: "{{certificate_list}}"}
	tid, err := a.createTemplate(ctx, "app_demo", input)
	if err != nil {
		t.Fatal(err)
	}
	mapping := Mapping{Email: 0, School: 1, Award: 2, URL: 3, Event: -1, CombineRecipients: true, CombinedTemplate: tid, NameColumns: []int{4}}
	if err := a.mapCampaign(ctx, cid, mapping); err == nil {
		t.Fatal("first-item award wording was allowed in combined subject")
	}
	mapping.CombinedTemplate = "tpl_welcome"
	if err := a.mapCampaign(ctx, cid, mapping); err == nil {
		t.Fatal("template without certificate list accepted")
	}
	mapping.NameColumns = nil
	if err := a.mapCampaign(ctx, cid, mapping); err == nil {
		t.Fatal("grouping without participant identities accepted")
	}
}
