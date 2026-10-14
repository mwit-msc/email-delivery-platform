package edp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalCertificateScanQueueAndApproval(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var actor string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&actor)
	key := automationKey(t, a, "app_demo", []string{"campaign.edit", "campaign.view", "template.edit"}, actor)
	tpl := automationObject(t, automationRequest(t, a, key, "POST", "/v1/templates", TemplateInput{Slug: "synthetic.content", Name: "Content", Subject: "Certificate", HTML: `<p>{{school_name}}</p><a href="{{certificate_url}}">PDF</a>`}), 201)["id"].(string)
	id := automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns", map[string]any{"name": "Local content", "kind": "certificate"}), 201)["id"].(string)
	rows := [][]string{{"email", "school", "name", "certificate_id", "url"}, {"alice@example.test", "Example School", "Alice Example", "CERT-001", ""}, {"bob@example.test", "Example School", "Bob Example", "CERT-002", ""}}
	if err := a.replaceCampaignSource(ctx, id, rows); err != nil {
		t.Fatal(err)
	}
	links := certificateLinks{Folder: "https://drive.google.com/drive/folders/example", URLColumn: 4, Columns: []int{2, 3}, Files: []drivePDF{{ID: "one", Name: "001.pdf"}, {ID: "two", Name: "002.pdf"}}, AutoThreshold: 95, Actor: actor}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = queueCertificateScan(ctx, tx, id, &links); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	raw, _ := json.Marshal(links)
	_, err = tx.Exec(ctx, "UPDATE campaigns SET certificate_links=$2 WHERE id=$1", id, raw)
	if err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mapping := Mapping{Email: 0, School: 1, URL: 4, Event: -1, Award: -1, DefaultAward: "Participation", Rules: []AwardRule{{Award: "Participation", Template: tpl}}}
	if err = a.mapCampaign(ctx, id, mapping); err != nil {
		t.Fatal(err)
	}
	if err = a.approveCampaign(ctx, id, actor); err == nil || !strings.Contains(err.Error(), "local certificate scan") {
		t.Fatal("scan did not block approval", err)
	}
	first, err := a.claimCertificateScan(ctx)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	second, err := a.claimCertificateScan(ctx)
	if err != nil || second == nil || first.ID == second.ID {
		t.Fatal(second, err)
	}
	if extra, err := a.claimCertificateScan(ctx); err != nil || extra != nil {
		t.Fatal("global PDF concurrency limit", extra, err)
	}
	if _, err = a.DB.Exec(ctx, "UPDATE certificate_scan_files SET lease_until=$2 WHERE id=$1", first.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	resumed, err := a.claimCertificateScan(ctx)
	if err != nil || resumed == nil || resumed.ID != first.ID || resumed.Lease == first.Lease {
		t.Fatal("interrupted PDF did not resume with a fresh lease", resumed, err)
	}
	tag, err := a.DB.Exec(ctx, "UPDATE certificate_scan_files SET state='done' WHERE id=$1 AND lease_token=$2", first.ID, first.Lease)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatal("stale PDF worker overwrote lease", err)
	}
	for _, job := range []*certificateScanJob{resumed, second} {
		text := "Alice Example CERT-001"
		if job.File == "two" {
			text = "Bob Example CERT-002"
		}
		encrypted := seal(a.C.Secret, text)
		if strings.Contains(encrypted, "Alice") {
			t.Fatal("plaintext cache")
		}
		if _, err = a.DB.Exec(ctx, "UPDATE certificate_scan_files SET state='done',text_enc=$2,method='text',pages=1,quality=100,finished_at=now(),lease_until=NULL WHERE id=$1", job.ID, encrypted); err != nil {
			t.Fatal(err)
		}
	}
	a.finalizeCertificateScans(ctx)
	if err = a.DB.QueryRow(ctx, "SELECT certificate_links,source_rows FROM campaigns WHERE id=$1", id).Scan(&raw, new([]byte)); err != nil {
		t.Fatal(err)
	}
	links, err = decodeLinks(raw)
	if err != nil || links.ScanState != "complete" || len(links.Matches) != 2 || links.Matches[0].Status != "accepted" || links.Matches[1].Score != 100 {
		t.Fatal("local content scan did not finalize and auto-accept", links, err)
	}
	if err = a.approveCampaign(ctx, id, actor); err != nil {
		t.Fatal("content-backed approval failed", err)
	}
	var queued int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1", id).Scan(&queued)
	if queued != 0 {
		t.Fatal("link approval queued email")
	}
	r := httptest.NewRequest("GET", "/campaigns/"+id+"/certificate-text/one", nil)
	r.AddCookie(runtimeTestSession(t, a, true))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Alice Example") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("scoped extracted text review unavailable", w.Code, w.Body.String())
	}
	if _, err = a.DB.Exec(ctx, "UPDATE campaigns SET state='draft' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err = a.replaceCampaignSource(ctx, id, rows); err != nil {
		t.Fatal(err)
	}
	a.DB.QueryRow(ctx, "SELECT count(*) FROM certificate_scan_files WHERE campaign_id=$1", id).Scan(&queued)
	if queued != 0 {
		t.Fatal("source replacement retained stale scan content")
	}
}
func TestDrivePDFDownloadUsesFixedEndpoint(t *testing.T) {
	client := &http.Client{Transport: driveRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" || r.URL.Path != "/drive/v3/files/synthetic" || r.URL.Query().Get("alt") != "media" {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("%PDF-1.4\n"))}, nil
	})}
	if data, err := downloadCertificatePDF(context.Background(), client, "synthetic"); err != nil || !strings.HasPrefix(string(data), "%PDF-") {
		t.Fatal(string(data), err)
	}
	if _, err := downloadCertificatePDF(context.Background(), client, "../private"); err == nil {
		t.Fatal("arbitrary download path accepted")
	}
	client.Transport = driveRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("private provider details"))}, nil
	})
	if _, err := downloadCertificatePDF(context.Background(), client, "synthetic"); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("provider error leaked", err)
	}
}
