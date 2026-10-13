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

func TestReusableAwardMappingAndArchiving(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	token := automationKey(t, a, "app_demo", []string{"template.view", "template.edit", "campaign.view", "campaign.edit", "campaign.approve"}, root)
	view := automationKey(t, a, "app_demo", []string{"template.view"}, root)
	input := TemplateInput{Slug: "synthetic.reusable", Name: "Reusable certificate", Subject: "Certificate {{award_text}}", HTML: `<p style="font-family:'Noto Sans Thai',Tahoma">{{school_name}}: {{award_text}}</p><a href="{{certificate_url}}">File</a>`, Plain: "{{school_name}}: {{award_text}}"}
	tid := automationObject(t, automationRequest(t, a, token, "POST", "/v1/templates", input), 201)["id"].(string)
	cid := automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns", map[string]any{"name": "Reusable", "kind": "certificate"}), 201)["id"].(string)
	rows := [][]string{{"email", "school", "event", "award", "link", "wording"}, {"one@example.test", "Synthetic school", "Specific", "Winner", "https://example.test/one", "Column value"}, {"two@example.test", "Other school", "Other", "Winner", "https://example.test/two", "Other column"}, {"three@example.test", "Third school", "Specific", "Participation", "https://example.test/three", "Column value"}}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+cid+"/import", map[string]any{"rows": rows}), 200)
	mapping := Mapping{Email: 0, School: 1, Event: 2, Award: 3, URL: 4, Columns: map[string]int{"award_text": 5}, Constants: map[string]string{"award_text": "Constant"}, Rules: []AwardRule{
		{Award: "Winner", Template: tid, Variables: map[string]string{"award_text": "Generic winner"}},
		{Event: "Specific", Award: "Winner", Template: tid, Variables: map[string]string{"award_text": "<img src=x onerror=evil()>"}},
		{Award: "Participation", Template: tid, Variables: map[string]string{"award_text": "Participation"}},
	}}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+cid+"/mapping", mapping), 200)
	stored, err := a.query(ctx, "SELECT ordinal,variables,errors,template_id FROM campaign_rows WHERE campaign_id=$1 ORDER BY ordinal", cid)
	if err != nil || len(stored) != 3 {
		t.Fatal("reusable mapping failed", err)
	}
	for i, expected := range []string{"<img src=x onerror=evil()>", "Generic winner", "Participation"} {
		var vars map[string]string
		raw, _ := json.Marshal(stored[i]["variables"])
		json.Unmarshal(raw, &vars)
		if vars["award_text"] != expected || stored[i]["template_id"] != tid {
			t.Fatal("event rule precedence or single-template reuse lost", vars)
		}
		rendered, err := a.renderMessage(ctx, "app_demo", tid, 1, vars)
		if err != nil || strings.Contains(rendered.HTML, "<img src=x") {
			t.Fatal("award parameter was not safely escaped", err)
		}
	}
	// The HTML form must retain API-only mappings while editing award text.
	fields := url.Values{"csrf": {"runtime-csrf"}, "action": {"map"}, "email": {"0"}, "school": {"1"}, "event": {"2"}, "award": {"3"}, "url": {"4"}, "rule.Winner": {tid}, "rule.Participation": {tid}, "rule_vars.Winner": {`{"award_text":"Winner from form"}`}}
	req := httptest.NewRequest("POST", "/campaigns/"+cid, strings.NewReader(fields.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(runtimeTestSession(t, a, true))
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusSeeOther {
		t.Fatal("award variable form failed", response.Code, response.Body.String())
	}
	var savedMapping Mapping
	var mappingJSON []byte
	a.DB.QueryRow(ctx, "SELECT mapping FROM campaigns WHERE id=$1", cid).Scan(&mappingJSON)
	json.Unmarshal(mappingJSON, &savedMapping)
	if savedMapping.Columns["award_text"] != 5 || savedMapping.Constants["award_text"] != "Constant" || selectedAwardRule(savedMapping.Rules, "Specific", "Winner").Variables["award_text"] != "Winner from form" {
		t.Fatal("form lost hidden column mappings or its per-award text")
	}
	// Per-award wording is part of the frozen campaign variables.
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+cid+"/approve", map[string]any{}), 200)
	if automationRequest(t, a, token, "POST", "/v1/campaigns/"+cid+"/mapping", mapping).Code == 200 {
		t.Fatal("approved campaign could be remapped")
	}
	if automationRequest(t, a, view, "POST", "/v1/templates/"+tid+"/archive", map[string]any{"archived": true}).Code != http.StatusForbidden {
		t.Fatal("viewer archived a template")
	}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/templates/"+tid+"/archive", map[string]any{"archived": true}), 200)
	list := automationObject(t, automationRequest(t, a, token, "GET", "/v1/templates", nil), 200)
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), tid) {
		t.Fatal("archive stayed in default template gallery")
	}
	list = automationObject(t, automationRequest(t, a, token, "GET", "/v1/templates?include_archived=true", nil), 200)
	raw, _ = json.Marshal(list)
	if !strings.Contains(string(raw), tid) {
		t.Fatal("archived template could not be retrieved for restoration")
	}
	preview := automationObject(t, automationRequest(t, a, token, "POST", "/v1/templates/"+tid+"/preview", map[string]any{"version": 1, "variables": map[string]string{"school_name": "Synthetic", "certificate_url": "https://example.test/file", "award_text": "Winner"}}), 200)
	if !strings.Contains(preview["document"].(string), "@font-face") || strings.Contains(preview["html"].(string), "@font-face") {
		t.Fatal("archive broke frozen rendering or font CSS leaked into editable content")
	}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/templates/"+tid+"/archive", map[string]any{"archived": false}), 200)
	// Changing the archive flag never creates/replaces an immutable version.
	var version int
	a.DB.QueryRow(ctx, "SELECT current_version FROM templates WHERE id=$1", tid).Scan(&version)
	if version != 1 {
		t.Fatal("archive changed a saved version")
	}
	if _, err = a.DB.Exec(ctx, "INSERT INTO applications(id,name,from_address) VALUES('other_app','Other','other@example.test')"); err != nil {
		t.Fatal(err)
	}
	other := automationKey(t, a, "other_app", []string{"template.edit"}, root)
	if automationRequest(t, a, other, "POST", "/v1/templates/"+tid+"/archive", map[string]any{"archived": true}).Code != http.StatusNotFound {
		t.Fatal("archive crossed application scope")
	}
}

func TestAwardRulePrecedenceIndependentOfOrder(t *testing.T) {
	specific := AwardRule{Event: "event", Award: "winner", Template: "specific", Variables: map[string]string{"award_text": "Specific"}}
	generic := AwardRule{Award: "winner", Template: "generic", Variables: map[string]string{"award_text": "Generic"}}
	for _, rules := range [][]AwardRule{{generic, specific}, {specific, generic}} {
		if rule := selectedAwardRule(rules, "event", "winner"); rule == nil || rule.Template != "specific" || rule.Variables["award_text"] != "Specific" {
			t.Fatal("specific rule lost to a generic rule")
		}
	}
}
