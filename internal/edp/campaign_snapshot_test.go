package edp

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCampaignSourceAndCompletedConfiguration(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	if err := a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root); err != nil {
		t.Fatal(err)
	}
	key := automationKey(t, a, "app_demo", []string{"campaign.view", "campaign.edit"}, root)
	cid := automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns", map[string]any{"name": "Snapshot review", "kind": "bulk"}), 201)["id"].(string)
	source := [][]string{{"email", "Display name"}, {"person@example.test", "=Synthetic formula"}, {"invalid", "Excluded source"}}
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/import", map[string]any{"rows": source}), 200)
	if err := a.mapBulkCampaign(ctx, cid, "tpl_welcome", 0, map[string]int{"name": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, "UPDATE campaign_rows SET excluded=true WHERE campaign_id=$1 AND ordinal=3", cid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, "UPDATE campaigns SET state='completed' WHERE id=$1", cid); err != nil {
		t.Fatal(err)
	}
	path := "/v1/campaigns/" + cid + "/source"
	response := automationRequest(t, a, key, "GET", path, nil)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("source download failed", response.Code, response.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(response.Body.String(), "\ufeff"))).ReadAll()
	if err != nil || len(records) != 3 || records[1][1] != "'=Synthetic formula" || records[2][1] != "Excluded source" {
		t.Fatal("source rows missing or unsafe", records, err)
	}
	obj := automationObject(t, automationRequest(t, a, key, "GET", path+"?format=json", nil), 200)
	raw, _ := json.Marshal(obj["rows"])
	var exact [][]string
	if err := json.Unmarshal(raw, &exact); err != nil || !reflect.DeepEqual(source, exact) {
		t.Fatal("JSON changed snapshot strings", exact, err)
	}
	if code := automationRequest(t, a, key, "GET", path+"?format=xlsx", nil).Code; code != 400 {
		t.Fatal("unsupported format accepted", code)
	}
	limited := automationKey(t, a, "app_demo", []string{"message.view"}, root)
	if code := automationRequest(t, a, limited, "GET", path, nil).Code; code != 403 {
		t.Fatal("source permission missing", code)
	}
	if _, err := a.DB.Exec(ctx, "INSERT INTO applications(id,name,from_address) VALUES('app_other','Other','sender@example.test')"); err != nil {
		t.Fatal(err)
	}
	other := automationKey(t, a, "app_other", []string{"campaign.view"}, root)
	if code := automationRequest(t, a, other, "GET", path, nil).Code; code != 403 {
		t.Fatal("source crossed workspace boundary", code)
	}
	session := runtimeTestSession(t, a, true)
	req := httptest.NewRequest("GET", "/campaigns/"+cid, nil)
	req.AddCookie(session)
	page := httptest.NewRecorder()
	a.Handler().ServeHTTP(page, req)
	for _, label := range []string{"Saved campaign configuration", "Column 2 · Display name", "Version 1", "Download source CSV", "Blind copy recipients (BCC)"} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), label) {
			t.Fatal("completed review lacks", label, page.Code)
		}
	}
}

func TestCampaignMappingReviewDoesNotGuessHistory(t *testing.T) {
	fields, raw := campaignMappingReview("bulk", map[string]any{}, []string{"email", "name"})
	if len(fields) != 0 || raw != "" {
		t.Fatal("invented historical bulk mapping")
	}
}
