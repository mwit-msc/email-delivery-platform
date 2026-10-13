package edp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBCCAdmissionAndCampaignFreeze(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	if _, err := a.DB.Exec(ctx, "UPDATE applications SET recipient_hour_limit=1,daily_limit=100 WHERE id='app_demo'"); err != nil {
		t.Fatal(err)
	}
	input := SendInput{ApplicationID: "app_demo", To: "one@example.test", BCC: []string{"copy@example.test"}, Template: "welcome", Variables: map[string]string{"name": "One"}}
	id, replay, err := a.Submit(ctx, input, "bcc-admission")
	if err != nil || replay {
		t.Fatal(err)
	}
	if _, replay, err = a.Submit(ctx, input, "bcc-admission"); err != nil || !replay {
		t.Fatal("BCC idempotent replay failed", err)
	}
	input.BCC = []string{"different@example.test"}
	if _, _, err = a.Submit(ctx, input, "bcc-admission"); err == nil {
		t.Fatal("BCC payload change did not conflict")
	}
	input.To, input.BCC = "two@example.test", []string{"copy@example.test"}
	if _, _, err = a.Submit(ctx, input, "bcc-recipient-quota"); err == nil {
		t.Fatal("BCC recipient cooldown bypassed")
	}
	var stored []string
	a.DB.QueryRow(ctx, "SELECT bcc FROM messages WHERE id=$1", id).Scan(&stored)
	if !reflect.DeepEqual(stored, []string{"copy@example.test"}) {
		t.Fatal("BCC was not persisted", stored)
	}
	// A new recipient with a BCC consumes two daily recipient deliveries.
	a.DB.Exec(ctx, "UPDATE applications SET daily_limit=3,recipient_hour_limit=100 WHERE id='app_demo'")
	input.To, input.BCC = "three@example.test", []string{"another@example.test"}
	if _, _, err = a.Submit(ctx, input, "bcc-daily-quota"); err == nil {
		t.Fatal("BCC daily volume bypassed")
	}
	a.DB.Exec(ctx, "UPDATE applications SET daily_limit=100,recipient_hour_limit=100 WHERE id='app_demo'")
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	key := automationKey(t, a, "app_demo", []string{"campaign.view", "campaign.edit", "campaign.approve", "campaign.send"}, root)
	viewer := automationKey(t, a, "app_demo", []string{"campaign.view"}, root)
	cid := automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns", map[string]any{"name": "BCC campaign", "kind": "bulk", "bcc": []string{"campaign-copy@example.test"}}), 201)["id"].(string)
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/import", map[string]any{"rows": [][]string{{"email", "name"}, {"first@example.test", "First"}, {"second@example.test", "Second"}}}), 200)
	if err := a.mapBulkCampaign(ctx, cid, "tpl_welcome", 0, map[string]int{"name": 1}, nil); err != nil {
		t.Fatal(err)
	}
	var mappingJSON []byte
	if err := a.DB.QueryRow(ctx, "SELECT mapping FROM campaigns WHERE id=$1", cid).Scan(&mappingJSON); err != nil {
		t.Fatal(err)
	}
	var savedMapping BulkMapping
	if err := json.Unmarshal(mappingJSON, &savedMapping); err != nil || savedMapping.Template != "tpl_welcome" || savedMapping.Email != 0 || savedMapping.Columns["name"] != 1 {
		t.Fatal("bulk mapping not preserved for configuration review", savedMapping, err)
	}
	path := "/v1/campaigns/" + cid + "/bcc"
	if automationRequest(t, a, viewer, "POST", path, map[string]any{"bcc": []string{}}).Code != http.StatusForbidden {
		t.Fatal("viewer changed BCC")
	}
	if automationRequest(t, a, key, "POST", path, map[string]any{}).Code != http.StatusBadRequest {
		t.Fatal("missing BCC array cleared copies")
	}
	automationObject(t, automationRequest(t, a, key, "POST", path, map[string]any{"bcc": []string{"copy-campaign@example.test"}}), 200)
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/approve", map[string]any{}), 200)
	if automationRequest(t, a, key, "POST", path, map[string]any{"bcc": []string{}}).Code != http.StatusBadRequest {
		t.Fatal("approval did not freeze BCC")
	}
	var count int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1", cid).Scan(&count)
	if count != 0 {
		t.Fatal("saving/approving BCC queued mail")
	}
	// The shared BCC mailbox is included in the campaign's atomic reservation.
	a.DB.Exec(ctx, "UPDATE applications SET recipient_hour_limit=1 WHERE id='app_demo'")
	if err := a.startCampaign(ctx, cid, time.Now()); err == nil {
		t.Fatal("shared campaign BCC bypassed recipient quota")
	}
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1", cid).Scan(&count)
	if count != 0 {
		t.Fatal("quota rejection partially queued campaign")
	}
	a.DB.Exec(ctx, "UPDATE applications SET recipient_hour_limit=100 WHERE id='app_demo'")
	if err := a.startCampaign(ctx, cid, time.Now()); err != nil {
		t.Fatal(err)
	}
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1 AND bcc=ARRAY['copy-campaign@example.test']", cid).Scan(&count)
	if count != 2 {
		t.Fatal("frozen BCC not copied into delivery jobs", count)
	}
	// Test messages use the supplied test mailbox alone, even with campaign BCC.
	if err := a.actCampaign(ctx, cid, "app_demo", root, "test", CampaignAction{To: "test-only@example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE recipient='test-only@example.test' AND cardinality(cc)=0 AND cardinality(bcc)=0 AND campaign_id IS NULL").Scan(&count); err != nil || count != 1 {
		t.Fatal("test message copied campaign BCC or joined the campaign queue", count, err)
	}
}

func TestBCCSMTPEnvelopeAndPartialRejection(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "BCC rejected"}[reject], func(t *testing.T) {
			a := testApp(t)
			ctx := context.Background()
			reply := ""
			if reject {
				reply = "550 5.1.1 Invalid copy recipient"
			}
			fixture := newRuntimeSMTPFixture(t, reply, "", "", "copy@example.test")
			a.C.SMTPAddress, a.C.SMTPTLS = fixture.Address, "none"
			id, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "to@example.test", BCC: []string{"copy@example.test"}, Template: "welcome", Variables: map[string]string{"name": "To"}}, "smtp-bcc")
			if err != nil {
				t.Fatal(err)
			}
			d, err := a.claim(ctx)
			if err != nil || d == nil || d.ID != id || len(d.BCC) != 1 {
				t.Fatal("claimed BCC was lost", err)
			}
			status, _ := a.smtpSend(ctx, *d)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if !reflect.DeepEqual(fixture.Recipients, []string{"to@example.test", "copy@example.test"}) {
				t.Fatal("SMTP did not issue both envelope recipients", fixture.Recipients)
			}
			if reject {
				if status != "failed" || fixture.Data.Load() != 0 {
					t.Fatal("partial recipient rejection submitted DATA", status)
				}
			} else if status != "relay_accepted" || fixture.Data.Load() != 1 || strings.Contains(fixture.Bodies[0], "copy@example.test") {
				t.Fatal("SMTP copy was not submitted", status)
			}
		})
	}
}

func TestBCCFallbackDoesNotDuplicateAcceptedCopies(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	a.C.SecondarySMTPAddress = "secondary.example.test:587"
	a.DB.Exec(ctx, "UPDATE applications SET fallback_enabled=true WHERE id='app_demo'")
	input := SendInput{ApplicationID: "app_demo", To: "primary@example.test", BCC: []string{"copy@example.test"}, Template: "welcome", Variables: map[string]string{"name": "Primary"}}
	id, _, err := a.Submit(ctx, input, "bcc-remote-fallback")
	if err != nil {
		t.Fatal(err)
	}
	a.DB.Exec(ctx, "UPDATE messages SET status='relay_accepted',relay_accepted_at=now() WHERE id=$1", id)
	if err = a.applyGatewayOutcome(ctx, id, "synthetic-bcc-bounce", "bounced", "550 5.7.1 IP blocked by Spamhaus", "ip_reputation"); err != nil {
		t.Fatal(err)
	}
	var route, status string
	var used bool
	a.DB.QueryRow(ctx, "SELECT route,status,secondary_used FROM messages WHERE id=$1", id).Scan(&route, &status, &used)
	if route != "primary" || status != "bounced" || used {
		t.Fatal("To rejection retried a possibly delivered BCC copy")
	}
	// Before the relay accepts DATA, no recipient has received a copy.
	input.To = "direct@example.test"
	id, _, err = a.Submit(ctx, input, "bcc-direct-fallback")
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.claim(ctx)
	if err != nil || d == nil || d.ID != id {
		t.Fatal(err)
	}
	if err = a.finish(ctx, *d, "failed", "550 5.7.1 IP blocked by Spamhaus"); err != nil {
		t.Fatal(err)
	}
	a.DB.QueryRow(ctx, "SELECT route,status,secondary_used FROM messages WHERE id=$1", id).Scan(&route, &status, &used)
	if route != "secondary" || status != "queued" || !used {
		t.Fatal("confirmed rejection before DATA lost safe fallback")
	}
}

func TestBCCNormalizationPrivacyAndCombinedLimit(t *testing.T) {
	cc, bcc, err := normalizeCopies("to@example.test", []string{"cc@EXAMPLE.test"}, []string{"blind@EXAMPLE.test", " CC@example.test ", "TO@example.test", "blind@example.test"})
	if err != nil || !reflect.DeepEqual(cc, []string{"cc@example.test"}) || !reflect.DeepEqual(bcc, []string{"blind@example.test"}) {
		t.Fatal("copy normalization failed", cc, bcc, err)
	}
	for _, input := range [][]string{{"Name <blind@example.test>"}, {"blind@example.test\r\nBcc: attacker@example.test"}, {"invalid"}} {
		if _, _, err := normalizeCopies("", nil, input); err == nil {
			t.Fatal("invalid BCC accepted")
		}
	}
	if _, _, err := normalizeCopies("", make([]string, 11), make([]string, 10)); err == nil {
		t.Fatal("combined copy recipient limit bypassed")
	}
	raw := string(makeMIME(delivery{ID: "synthetic", From: "sender@example.test", Recipient: "to@example.test", CC: cc, BCC: bcc, Subject: "Subject", HTML: "<p>Body</p>", Plain: "Body"}, "https://edp.example.test"))
	if strings.Contains(strings.ToLower(raw), "bcc:") || strings.Contains(raw, "blind@example.test") || !strings.Contains(raw, "Cc: cc@example.test") {
		t.Fatal("blind recipient leaked into MIME or visible CC disappeared")
	}
}

func TestBCCConcurrentRecipientQuota(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	if _, err := a.DB.Exec(ctx, "UPDATE applications SET recipient_hour_limit=1,daily_limit=100 WHERE id='app_demo'"); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, to := range []string{"first@example.test", "second@example.test"} {
		go func(to string) {
			<-ready
			_, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: to, BCC: []string{"shared@example.test"}, Template: "welcome", Variables: map[string]string{"name": "Synthetic"}}, to)
			results <- err
		}(to)
	}
	close(ready)
	accepted := 0
	for range 2 {
		if err := <-results; err == nil {
			accepted++
		} else if httpError, ok := err.(*HTTPError); !ok || httpError.Code != http.StatusTooManyRequests {
			t.Fatal("unexpected admission failure", err)
		}
	}
	if accepted != 1 {
		t.Fatal("concurrent BCC admissions bypassed shared recipient quota", accepted)
	}
}

func TestCampaignCopyEditsAreSerialized(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	if err := a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root); err != nil {
		t.Fatal(err)
	}
	key := automationKey(t, a, "app_demo", []string{"campaign.edit"}, root)
	cid := automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns", map[string]any{"name": "Concurrent copies", "kind": "bulk"}), 201)["id"].(string)
	ready := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-ready
		results <- a.setCampaignCC(ctx, cid, []string{"visible@example.test"})
	}()
	go func() {
		<-ready
		results <- a.setCampaignBCC(ctx, cid, []string{"blind@example.test"})
	}()
	close(ready)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var cc, bcc []string
	if err := a.DB.QueryRow(ctx, "SELECT cc,bcc FROM campaigns WHERE id=$1", cid).Scan(&cc, &bcc); err != nil || !reflect.DeepEqual(cc, []string{"visible@example.test"}) || !reflect.DeepEqual(bcc, []string{"blind@example.test"}) {
		t.Fatal("concurrent copy edits lost another list", cc, bcc, err)
	}
	// Rejecting an oversized unique combined list must preserve both saved lists.
	large := make([]string, 20)
	for i := range large {
		large[i] = fmt.Sprintf("copy%d@example.test", i)
	}
	if err := a.setCampaignBCC(ctx, cid, large); err == nil {
		t.Fatal("campaign exceeded combined copy limit")
	}
	if err := a.DB.QueryRow(ctx, "SELECT cc,bcc FROM campaigns WHERE id=$1", cid).Scan(&cc, &bcc); err != nil || !reflect.DeepEqual(bcc, []string{"blind@example.test"}) {
		t.Fatal("failed copy edit changed saved list", err)
	}
}

func TestBCCCombinedLimitAfterDeduplication(t *testing.T) {
	copies := make([]string, 20)
	for i := range copies {
		copies[i] = fmt.Sprintf("copy%d@example.test", i)
	}
	cc, bcc, err := normalizeCopies("primary@example.test", copies, copies)
	if err != nil || len(cc) != 20 || len(bcc) != 0 {
		t.Fatal("duplicate copies consumed combined capacity", cc, bcc, err)
	}
	if _, _, err := normalizeCopies("primary@example.test", copies, []string{"extra@example.test"}); err == nil {
		t.Fatal("21 unique copies accepted")
	}
}
