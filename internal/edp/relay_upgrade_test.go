package edp

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNamedRelayUpgradePreservesConfigurationAndQueue(t *testing.T) {
	a := releasedSchemaApp(t)
	ctx := context.Background()
	for _, filename := range []string{"007_campaign_automation.sql", "008_named_relays.sql", "009_template_archiving.sql", "010_certificate_groups.sql", "011_cc_recipients.sql", "012_sender_display_name.sql"} {
		data, err := resources.ReadFile("migrations/" + filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.DB.Exec(ctx, string(data)); err != nil {
			t.Fatal(filename, err)
		}
	}
	primary := newRuntimeSMTPFixture(t, "", "upgrade-user", "upgrade-secret")
	secondary := newRuntimeSMTPFixture(t, "", "fallback-user", "fallback-secret")
	c := a.C
	c.SMTPAddress, c.SMTPUser, c.SMTPPassword = primary.Address, "upgrade-user", "upgrade-secret"
	c.SecondarySMTPAddress, c.SecondarySMTPUser, c.SecondarySMTPPassword, c.SecondarySMTPTLS = secondary.Address, "fallback-user", "fallback-secret", "none"
	c.BounceDomain = "bounce.example.test"
	if err := a.saveRuntime(ctx, "smtp", c); err != nil {
		t.Fatal(err)
	}
	if err := a.saveRuntime(ctx, "secondary", c); err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := a.DB.QueryRow(ctx, "SELECT runtime_config->'smtp'->>'password_enc' FROM platform_settings WHERE id=1").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	custom, err := a.saveRelay(ctx, RelayInput{Name: "Existing relay", Address: "existing.example.test:587", TLS: "starttls", Enabled: true, AllowedDomains: []string{"other.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.setDomainRoute(ctx, "other.test", custom); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(ctx, "INSERT INTO applications(id,name,from_address) VALUES('other_app','Other','sender@other.test')"); err != nil {
		t.Fatal(err)
	}
	id, secondaryID := newID("msg_"), newID("msg_")
	for _, mid := range []string{id, secondaryID} {
		if _, err = a.DB.Exec(ctx, `INSERT INTO messages(id,application_id,recipient,from_address,subject,html,plain,class,idempotency_key,payload_hash,expires_at) VALUES($1,'app_demo','to@example.test','notifications@example.test','Synthetic','<p>Synthetic</p>','Synthetic','transactional',$1,'snapshot',now()+interval '1 day')`, mid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.DB.Exec(ctx, "UPDATE applications SET fallback_enabled=true WHERE id='app_demo'"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(ctx, "UPDATE messages SET route='secondary',secondary_used=true WHERE id=$1", secondaryID); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err = a.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if primary.Data.Load() != 0 || secondary.Data.Load() != 0 {
		t.Fatal("migration sent email")
	}
	var count int
	var after string
	if err = a.DB.QueryRow(ctx, "SELECT count(*) FROM smtp_relays WHERE id IN($1,$2)", importedPrimaryRelay, importedSecondaryRelay).Scan(&count); err != nil || count != 2 {
		t.Fatal("imports missing or duplicated", count, err)
	}
	if err = a.DB.QueryRow(ctx, "SELECT password FROM smtp_relays WHERE id=$1", importedPrimaryRelay).Scan(&after); err != nil || after != encrypted {
		t.Fatal("encrypted credential changed")
	}
	if route, err := a.senderRelay(ctx, "sender@other.test"); err != nil || route != custom {
		t.Fatal("existing named route overwritten", err)
	}
	if _, err = a.senderRelay(ctx, "sender@unknown.test"); err == nil {
		t.Fatal("unmapped sender retained legacy fallback")
	}
	if err = a.setStrictRouting(ctx, false); err == nil {
		t.Fatal("retired fallback could be enabled")
	}
	if fallback, err := a.fallbackRelay(ctx, "notifications@example.test", importedPrimaryRelay); err != nil || fallback != importedSecondaryRelay {
		t.Fatal("fallback was not preserved", err)
	}
	if err = a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE status='queued' AND id=ANY($1) AND relay_id=$2", []string{id, secondaryID}, importedPrimaryRelay).Scan(&count); err != nil || count != 2 {
		t.Fatal("pending message state changed", count, err)
	}
	var fallbackID string
	if err = a.DB.QueryRow(ctx, "SELECT fallback_relay_id FROM messages WHERE id=$1", secondaryID).Scan(&fallbackID); err != nil || fallbackID != importedSecondaryRelay {
		t.Fatal("pending fallback was lost", err)
	}
	root := runtimeTestSession(t, a, true)
	if w := runtimeSettingsRequest(a, root, url.Values{"action": {"smtp.save"}, "address": {"changed.example.test:587"}, "tls": {"starttls"}}, true); w.Code != http.StatusBadRequest {
		t.Fatal("retired config accepted", w.Code)
	}
	// Both queued routes still use their original authenticated SMTP endpoint.
	for n := 0; n < 2; n++ {
		d, err := a.claim(ctx)
		if err != nil || d == nil {
			t.Fatal("queued delivery could not be claimed", err)
		}
		status, detail := a.smtpSend(ctx, *d)
		if status != "relay_accepted" {
			t.Fatal(status, detail)
		}
		if err = a.finish(ctx, *d, status, detail); err != nil {
			t.Fatal(err)
		}
		a.DB.Exec(ctx, "UPDATE platform_settings SET next_send_at=now();UPDATE applications SET next_send_at=now()")
	}
	if primary.Data.Load() != 1 || secondary.Data.Load() != 1 {
		t.Fatal("upgrade changed SMTP endpoints")
	}
	var raw string
	if err = a.DB.QueryRow(ctx, "SELECT runtime_config::text FROM platform_settings WHERE id=1").Scan(&raw); err != nil || strings.Contains(raw, "password_enc") {
		t.Fatal("legacy runtime settings were not moved")
	}
}
