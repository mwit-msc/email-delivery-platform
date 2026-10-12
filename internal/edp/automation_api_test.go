package edp

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func automationKey(t *testing.T, a *App, app string, scopes []string, owner any) string {
	t.Helper()
	token := newID("synthetic_key_")
	_, err := a.DB.Exec(context.Background(), "INSERT INTO api_keys(id,application_id,name,token_hash,scopes,created_by) VALUES($1,$2,'Synthetic automation', $3,$4,$5)", newID("key_"), app, tokenHash(token), scopes, owner)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func automationRequest(t *testing.T, a *App, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, req)
	return response
}

func automationObject(t *testing.T, response *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if response.Code != status {
		t.Fatalf("expected HTTP %d; got %d: %s", status, response.Code, response.Body.String())
	}
	var object map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func automationUpload(t *testing.T, a *App, token, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	file.Write(data)
	writer.Close()
	req := httptest.NewRequest("POST", "/v1/assets", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, req)
	return response
}

func TestAutomationAPIWorkflow(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	if err := a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root); err != nil {
		t.Fatal(err)
	}
	token := automationKey(t, a, "app_demo", []string{"template.view", "template.edit", "campaign.view", "campaign.edit", "campaign.approve", "campaign.send", "message.send"}, root)
	input := TemplateInput{Slug: "synthetic.certificate", Name: "Synthetic certificate", Subject: "Certificate for {{school_name}}", HTML: `<div style="color:#5e5ce6;padding:20px"><p>{{school_name}}</p><a href="{{certificate_url}}">Certificate</a><script>UNSAFE_SCRIPT</script></div>`, Project: json.RawMessage(`{"pages":[{"component":{"tagName":"p","content":"Safe","script":"UNSAFE_PROJECT","attributes":{"onclick":"UNSAFE_EVENT"}}}]}`)}
	created := automationObject(t, automationRequest(t, a, token, "POST", "/v1/templates", input), 201)
	templateID := created["id"].(string)
	stored := automationObject(t, automationRequest(t, a, token, "GET", "/v1/templates/"+templateID, nil), 200)
	storedJSON, _ := json.Marshal(stored)
	if strings.Contains(string(storedJSON), "UNSAFE") || !strings.Contains(stored["html"].(string), "#5e5ce6") {
		t.Fatal("template sanitization removed safe styling or retained executable content")
	}
	unsaved := input
	unsaved.Subject = "Unsaved {{school_name}}"
	preview := automationObject(t, automationRequest(t, a, token, "POST", "/v1/templates/"+templateID+"/preview", map[string]any{"template": unsaved, "variables": map[string]string{"school_name": `<img src=x onerror=alert(1)>`, "certificate_url": "https://example.test/certificate"}}), 200)
	if preview["subject"] != "Unsaved <img src=x onerror=alert(1)>" || !strings.Contains(preview["html"].(string), "&lt;img") || strings.Contains(preview["html"].(string), "<img src=x") {
		t.Fatal("preview failed to escape substitution")
	}
	if response := automationRequest(t, a, token, "POST", "/v1/templates/"+templateID+"/preview", map[string]any{"variables": map[string]string{"school_name": "Synthetic"}}); response.Code != 400 {
		t.Fatal("missing certificate URL was accepted", stored["html"], response.Body.String())
	}
	if automationRequest(t, a, token, "POST", "/v1/templates/"+templateID+"/preview", map[string]any{"variables": map[string]string{"school_name": "Synthetic", "certificate_url": "javascript:alert(1)"}}).Code != 400 {
		t.Fatal("executable URL substitution accepted")
	}
	// Variables without a "url" suffix still cannot produce executable links:
	// the fully substituted HTML is sanitized again before preview or delivery.
	unsafeLink := input
	unsafeLink.HTML = `<a href="{{destination}}">Synthetic link</a>`
	linkPreview := automationObject(t, automationRequest(t, a, token, "POST", "/v1/templates/"+templateID+"/preview", map[string]any{"template": unsafeLink, "variables": map[string]string{"school_name": "Synthetic", "destination": "javascript:alert(1)"}}), 200)
	if strings.Contains(linkPreview["html"].(string), "javascript:") || strings.Contains(linkPreview["html"].(string), "href=") {
		t.Fatal("an arbitrary variable name bypassed final link sanitization")
	}
	input.Version = 1
	input.Subject = "Frozen version two {{school_name}}"
	automationObject(t, automationRequest(t, a, token, "PUT", "/v1/templates/"+templateID, input), 200)
	if automationRequest(t, a, token, "PUT", "/v1/templates/"+templateID, input).Code != 409 {
		t.Fatal("stale template version was not rejected")
	}
	versionOne := automationObject(t, automationRequest(t, a, token, "GET", "/v1/templates/"+templateID+"?version=1", nil), 200)
	if versionOne["subject"] != "Certificate for {{school_name}}" {
		t.Fatal("saving replaced a released template version")
	}

	campaign := automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns", map[string]any{"name": "Synthetic certificate dispatch", "kind": "certificate"}), 201)
	campaignID := campaign["id"].(string)
	rows := [][]string{{"Email", "School", "Event", "Award", "Certificate"}, {"synthetic-one@example.test", "โรงเรียนตัวอย่าง", "Synthetic event", "รางวัลตัวอย่าง", "https://example.test/one"}, {"invalid-recipient", "Synthetic school", "Synthetic event", "รางวัลตัวอย่าง", "https://example.test/two"}, {"", "", "", "Footer", ""}}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/import", map[string]any{"rows": rows}), 200)
	mapping := Mapping{Email: 0, School: 1, Event: 2, Award: 3, URL: 4, SkipRowsWithoutEmail: true, Rules: []AwardRule{{Event: "Synthetic event", Award: "รางวัลตัวอย่าง", Template: templateID}}}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/mapping", mapping), 200)
	loaded := automationObject(t, automationRequest(t, a, token, "GET", "/v1/campaigns/"+campaignID, nil), 200)
	mappedRows := loaded["rows"].([]any)
	if len(mappedRows) != 2 {
		t.Fatal("footer was not omitted or invalid recipient was silently dropped", len(mappedRows))
	}
	invalid := mappedRows[1].(map[string]any)
	if len(invalid["errors"].([]any)) == 0 || invalid["template_id"] != templateID || invalid["template_version"] != float64(2) {
		t.Fatal("arbitrary award rule or recipient validation failed", invalid)
	}
	if automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/approve", map[string]any{}).Code != 400 {
		t.Fatal("invalid recipient did not block approval")
	}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/exclude", CampaignAction{RowID: invalid["id"].(string), Excluded: true}), 200)
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/approve", map[string]any{}), 200)
	if automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/mapping", mapping).Code != 400 {
		t.Fatal("approved mapping was editable")
	}
	input.Version = 2
	input.Subject = "Changed version three {{school_name}}"
	automationObject(t, automationRequest(t, a, token, "PUT", "/v1/templates/"+templateID, input), 200)
	when := time.Now().Add(24 * time.Hour)
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/start", CampaignAction{ScheduleAt: &when}), 200)
	var subject, messageID string
	var available time.Time
	if err := a.DB.QueryRow(ctx, "SELECT id,subject,available_at FROM messages WHERE campaign_id=$1", campaignID).Scan(&messageID, &subject, &available); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(subject, "Frozen version two") || time.Until(available) < 23*time.Hour {
		t.Fatal("scheduled dispatch changed its approved template or timing")
	}
	progress := automationObject(t, automationRequest(t, a, token, "GET", "/v1/campaigns/"+campaignID+"/progress", nil), 200)
	statuses := progress["statuses"].([]any)
	if len(statuses) != 1 || statuses[0].(map[string]any)["status"] != "queued" || statuses[0].(map[string]any)["count"] != float64(1) {
		t.Fatal("progress did not report the durable scheduled job", progress)
	}
	if _, err := a.DB.Exec(ctx, "UPDATE messages SET last_error='=HYPERLINK(\"https://example.test\")' WHERE id=$1", messageID); err != nil {
		t.Fatal(err)
	}
	export := automationRequest(t, a, token, "GET", "/v1/campaigns/"+campaignID+"/export", nil)
	if export.Code != 200 || !strings.HasPrefix(export.Header().Get("Content-Type"), "text/csv") {
		t.Fatal("campaign export failed")
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(export.Body.String(), "\ufeff"))).ReadAll()
	if err != nil || len(records) != 3 || !strings.HasPrefix(records[1][8], "'=HYPERLINK") {
		t.Fatal("CSV export omitted rows or allowed a spreadsheet formula", records, err)
	}
	automationObject(t, automationRequest(t, a, token, "POST", "/v1/campaigns/"+campaignID+"/cancel", map[string]any{}), 200)
	var status, campaignState string
	a.DB.QueryRow(ctx, "SELECT status FROM messages WHERE id=$1", messageID).Scan(&status)
	a.DB.QueryRow(ctx, "SELECT state FROM campaigns WHERE id=$1", campaignID).Scan(&campaignState)
	if status != "cancelled" || campaignState != "cancelled" {
		t.Fatal("API cancellation left a scheduled delivery active")
	}

	t.Run("tenant and scope isolation", func(t *testing.T) {
		if _, err := a.DB.Exec(ctx, "INSERT INTO applications(id,name,from_address) VALUES('app_other','Other','sender@example.test')"); err != nil {
			t.Fatal(err)
		}
		other := automationKey(t, a, "app_other", []string{"template.view", "template.edit", "campaign.view", "campaign.edit", "campaign.send"}, root)
		for _, path := range []string{"/v1/templates/" + templateID, "/v1/campaigns/" + campaignID, "/v1/campaigns/" + campaignID + "/progress"} {
			if code := automationRequest(t, a, other, "GET", path, nil).Code; code != 404 {
				t.Fatalf("foreign resource visible at %s: HTTP %d", path, code)
			}
		}
		if code := automationRequest(t, a, other, "PUT", "/v1/templates/"+templateID, input).Code; code < 400 || code >= 500 {
			t.Fatal("foreign template editable", code)
		}
		if code := automationRequest(t, a, other, "POST", "/v1/campaigns/"+campaignID+"/cancel", map[string]any{}).Code; code != 404 {
			t.Fatal("foreign campaign actionable", code)
		}
		if automationRequest(t, a, "", "GET", "/v1/templates", nil).Code != 401 {
			t.Fatal("anonymous API request accepted")
		}
		limited := automationKey(t, a, "app_demo", []string{"message.view"}, root)
		if automationRequest(t, a, limited, "POST", "/v1/templates", input).Code != 403 || automationRequest(t, a, limited, "POST", "/v1/campaigns/"+campaignID+"/approve", map[string]any{}).Code != 403 {
			t.Fatal("missing API scope granted authority")
		}
	})
	t.Run("logo assets", func(t *testing.T) {
		var imageData bytes.Buffer
		if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		asset := automationObject(t, automationUpload(t, a, token, "synthetic-logo.png", imageData.Bytes()), 201)
		response := automationRequest(t, a, "", "GET", "/email-assets/"+asset["id"].(string), nil)
		if response.Code != 200 || response.Header().Get("Content-Type") != "image/png" || !bytes.Equal(response.Body.Bytes(), imageData.Bytes()) || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("public logo bytes, type, or response protection incorrect")
		}
		if automationUpload(t, a, token, "unsafe.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)).Code != 400 {
			t.Fatal("executable SVG uploaded as an email image")
		}
	})
	t.Run("legacy keys keep sending but require ownership for campaigns", func(t *testing.T) {
		legacy := automationKey(t, a, "app_demo", []string{"message.send", "campaign.edit"}, nil)
		response := automationRequest(t, a, legacy, "POST", "/v1/campaigns", map[string]any{"name": "Ownerless", "kind": "bulk"})
		if response.Code < 400 || response.Code >= 500 {
			t.Fatal("ownerless key must be rejected as a client error", response.Code, response.Body.String())
		}
		request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"to":"legacy@example.test","template":"welcome","variables":{"name":"Synthetic"}}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+legacy)
		request.Header.Set("Idempotency-Key", "legacy-api-send")
		response = httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		automationObject(t, response, 202)
	})
}

func automationSession(t *testing.T, a *App, user string) (*http.Cookie, string) {
	t.Helper()
	token, csrf := newID("test_session_"), newID("test_csrf_")
	if _, err := a.DB.Exec(context.Background(), "INSERT INTO sessions(token_hash,user_id,method,csrf,expires_at) VALUES($1,$2,'local',$3,now()+interval '1 hour')", tokenHash(token), user, csrf); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "edp_session", Value: token}, csrf
}

func TestAutomationKeyIssuancePermissions(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	if _, err := a.DB.Exec(ctx, "INSERT INTO users(id,username,name) VALUES('developer','synthetic-developer','Developer'); INSERT INTO bindings(user_id,scope,role) VALUES('developer','app_demo','developer')"); err != nil {
		t.Fatal(err)
	}
	developerCookie, developerCSRF := automationSession(t, a, "developer")
	post := func(cookie *http.Cookie, csrf, scope string) *httptest.ResponseRecorder {
		form := url.Values{"action": {"key"}, "name": {"Synthetic scope test"}, "csrf": {csrf}, "scopes": {scope}}
		req := httptest.NewRequest("POST", "/applications/app_demo", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, req)
		return response
	}
	for _, scope := range []string{"campaign.approve", "relay.manage"} {
		if response := post(developerCookie, developerCSRF, scope); response.Code != 403 {
			t.Fatalf("developer granted %s: HTTP %d", scope, response.Code)
		}
	}
	var count int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM api_keys").Scan(&count)
	if count != 0 {
		t.Fatal("rejected key request persisted a credential")
	}
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	rootCookie, rootCSRF := automationSession(t, a, root)
	for _, scope := range []string{"campaign.approve", "relay.manage"} {
		if response := post(rootCookie, rootCSRF, scope); response.Code != 200 {
			t.Fatalf("root could not provision %s: HTTP %d", scope, response.Code)
		}
	}
	a.DB.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE created_by=$1", root).Scan(&count)
	if count != 2 {
		t.Fatal("key issuance did not persist its actual owner")
	}
}
