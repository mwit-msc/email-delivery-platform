package edp

import (
	"bytes"
	"context"
	"net/http"
	"net/mail"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCCNormalizationAndHeaders(t *testing.T) {
	cc, err := normalizeCC("to@example.test", []string{"copy@EXAMPLE.test", " copy@example.test ", "TO@example.test", "other@example.test"})
	if err != nil || !reflect.DeepEqual(cc, []string{"copy@example.test", "other@example.test"}) {
		t.Fatal("CC normalization or duplicate To removal failed", cc, err)
	}
	for _, input := range [][]string{{"Name <copy@example.test>"}, {"copy@example.test\r\nBcc: attacker@example.test"}, {"not-an-address"}, make([]string, 21)} {
		if _, err := normalizeCC("", input); err == nil {
			t.Fatal("invalid CC input accepted")
		}
	}
	raw := makeMIME(delivery{ID: "synthetic", From: "sender@example.test", Recipient: "to@example.test", CC: cc, Subject: "Subject", HTML: "<p>Body</p>", Plain: "Body"}, "https://edp.example.test")
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := mail.ParseAddressList(message.Header.Get("Cc"))
	if err != nil || len(addresses) != 2 || addresses[0].Address != cc[0] || message.Header.Get("To") != "to@example.test" {
		t.Fatal("CC MIME header is invalid", addresses, err)
	}
}

func TestCCAdmissionAndCampaignFreeze(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	if _, err := a.DB.Exec(ctx, "UPDATE applications SET recipient_hour_limit=1,daily_limit=100 WHERE id='app_demo'"); err != nil {
		t.Fatal(err)
	}
	input := SendInput{ApplicationID: "app_demo", To: "one@example.test", CC: []string{"copy@example.test"}, Template: "welcome", Variables: map[string]string{"name": "One"}}
	id, replay, err := a.Submit(ctx, input, "cc-admission")
	if err != nil || replay {
		t.Fatal(err)
	}
	if _, replay, err = a.Submit(ctx, input, "cc-admission"); err != nil || !replay {
		t.Fatal("CC idempotent replay failed", err)
	}
	input.CC = []string{"different@example.test"}
	if _, _, err = a.Submit(ctx, input, "cc-admission"); err == nil {
		t.Fatal("CC payload change did not conflict")
	}
	input.To, input.CC = "two@example.test", []string{"copy@example.test"}
	if _, _, err = a.Submit(ctx, input, "cc-recipient-quota"); err == nil {
		t.Fatal("CC recipient cooldown bypassed")
	}
	var stored []string
	a.DB.QueryRow(ctx, "SELECT cc FROM messages WHERE id=$1", id).Scan(&stored)
	if !reflect.DeepEqual(stored, []string{"copy@example.test"}) {
		t.Fatal("CC was not persisted", stored)
	}
	// A new recipient with a CC consumes two daily recipient deliveries.
	a.DB.Exec(ctx, "UPDATE applications SET daily_limit=3,recipient_hour_limit=100 WHERE id='app_demo'")
	input.To, input.CC = "three@example.test", []string{"another@example.test"}
	if _, _, err = a.Submit(ctx, input, "cc-daily-quota"); err == nil {
		t.Fatal("CC daily volume bypassed")
	}
	a.DB.Exec(ctx, "UPDATE applications SET daily_limit=100,recipient_hour_limit=100 WHERE id='app_demo'")
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	key := automationKey(t, a, "app_demo", []string{"campaign.view", "campaign.edit", "campaign.approve", "campaign.send"}, root)
	viewer := automationKey(t, a, "app_demo", []string{"campaign.view"}, root)
	cid := automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns", map[string]any{"name": "CC campaign", "kind": "bulk", "cc": []string{"campaign-copy@example.test"}}), 201)["id"].(string)
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/import", map[string]any{"rows": [][]string{{"email", "name"}, {"first@example.test", "First"}, {"second@example.test", "Second"}}}), 200)
	if err := a.mapBulkCampaign(ctx, cid, "tpl_welcome", 0, map[string]int{"name": 1}, nil); err != nil {
		t.Fatal(err)
	}
	path := "/v1/campaigns/" + cid + "/cc"
	if automationRequest(t, a, viewer, "POST", path, map[string]any{"cc": []string{}}).Code != http.StatusForbidden {
		t.Fatal("viewer changed CC")
	}
	if automationRequest(t, a, key, "POST", path, map[string]any{}).Code != http.StatusBadRequest {
		t.Fatal("missing CC array cleared copies")
	}
	automationObject(t, automationRequest(t, a, key, "POST", path, map[string]any{"cc": []string{"copy-campaign@example.test"}}), 200)
	automationObject(t, automationRequest(t, a, key, "POST", "/v1/campaigns/"+cid+"/approve", map[string]any{}), 200)
	if automationRequest(t, a, key, "POST", path, map[string]any{"cc": []string{}}).Code != http.StatusBadRequest {
		t.Fatal("approval did not freeze CC")
	}
	var count int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1", cid).Scan(&count)
	if count != 0 {
		t.Fatal("saving/approving CC queued mail")
	}
	// The shared CC mailbox is included in the campaign's atomic reservation.
	a.DB.Exec(ctx, "UPDATE applications SET recipient_hour_limit=1 WHERE id='app_demo'")
	if err := a.startCampaign(ctx, cid, time.Now()); err == nil {
		t.Fatal("shared campaign CC bypassed recipient quota")
	}
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1", cid).Scan(&count)
	if count != 0 {
		t.Fatal("quota rejection partially queued campaign")
	}
	a.DB.Exec(ctx, "UPDATE applications SET recipient_hour_limit=100 WHERE id='app_demo'")
	if err := a.startCampaign(ctx, cid, time.Now()); err != nil {
		t.Fatal(err)
	}
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE campaign_id=$1 AND cc=ARRAY['copy-campaign@example.test']", cid).Scan(&count)
	if count != 2 {
		t.Fatal("frozen CC not copied into delivery jobs", count)
	}
	// Test messages use the supplied test mailbox alone, even with campaign CC.
	if err := a.actCampaign(ctx, cid, "app_demo", root, "test", CampaignAction{To: "test-only@example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE recipient='test-only@example.test' AND cardinality(cc)=0 AND campaign_id IS NULL").Scan(&count); err != nil || count != 1 {
		t.Fatal("test message copied campaign CC or joined the campaign queue", count, err)
	}
}

func TestCCSMTPEnvelopeAndPartialRejection(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "CC rejected"}[reject], func(t *testing.T) {
			a := testApp(t)
			ctx := context.Background()
			reply := ""
			if reject {
				reply = "550 5.1.1 Invalid copy recipient"
			}
			fixture := newRuntimeSMTPFixture(t, reply, "", "", "copy@example.test")
			a.C.SMTPAddress, a.C.SMTPTLS = fixture.Address, "none"
			id, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "to@example.test", CC: []string{"copy@example.test"}, Template: "welcome", Variables: map[string]string{"name": "To"}}, "smtp-cc")
			if err != nil {
				t.Fatal(err)
			}
			d, err := a.claim(ctx)
			if err != nil || d == nil || d.ID != id || len(d.CC) != 1 {
				t.Fatal("claimed CC was lost", err)
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
			} else if status != "relay_accepted" || fixture.Data.Load() != 1 || !strings.Contains(fixture.Bodies[0], "Cc: copy@example.test") {
				t.Fatal("SMTP copy was not submitted", status)
			}
		})
	}
}

func TestCCFallbackDoesNotDuplicateAcceptedCopies(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	a.C.SecondarySMTPAddress = "secondary.example.test:587"
	a.DB.Exec(ctx, "UPDATE applications SET fallback_enabled=true WHERE id='app_demo'")
	input := SendInput{ApplicationID: "app_demo", To: "primary@example.test", CC: []string{"copy@example.test"}, Template: "welcome", Variables: map[string]string{"name": "Primary"}}
	id, _, err := a.Submit(ctx, input, "cc-remote-fallback")
	if err != nil {
		t.Fatal(err)
	}
	a.DB.Exec(ctx, "UPDATE messages SET status='relay_accepted',relay_accepted_at=now() WHERE id=$1", id)
	if err = a.applyGatewayOutcome(ctx, id, "synthetic-cc-bounce", "bounced", "550 5.7.1 IP blocked by Spamhaus", "ip_reputation"); err != nil {
		t.Fatal(err)
	}
	var route, status string
	var used bool
	a.DB.QueryRow(ctx, "SELECT route,status,secondary_used FROM messages WHERE id=$1", id).Scan(&route, &status, &used)
	if route != "primary" || status != "bounced" || used {
		t.Fatal("To rejection retried a possibly delivered CC copy")
	}
	// Before the relay accepts DATA, no recipient has received a copy.
	input.To = "direct@example.test"
	id, _, err = a.Submit(ctx, input, "cc-direct-fallback")
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
