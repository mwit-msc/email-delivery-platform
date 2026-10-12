package edp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRelayDomainValidation(t *testing.T) {
	for _, input := range []string{"*.example.test", "example.test.", "-example.test", "a..test", "a@example.test", "example.test/path", "127.0.0.1", "ตัวอย่าง.test", strings.Repeat("a", 64) + ".test"} {
		if _, e := relayDomain(input); e == nil {
			t.Errorf("accepted invalid domain %q", input)
		}
	}
	for _, input := range []string{"a.com", "xn--o3cw4h.test", "sub.example.test", " EXAMPLE.TEST "} {
		if _, e := relayDomain(input); e != nil {
			t.Errorf("valid domain %q: %v", input, e)
		}
	}
	if domain, e := senderDomain(`"local@part"@EXAMPLE.TEST`); e != nil || domain != "example.test" {
		t.Fatalf("quoted sender domain: %q %v", domain, e)
	}
	base := RelayInput{Name: "Relay", Address: "localhost:1025", TLS: "none", AllowedDomains: []string{"example.test"}, Enabled: true}
	if _, e := normalizeRelay(base, false); e == nil {
		t.Fatal("production accepted plaintext SMTP")
	}
	for _, address := range []string{"localhost:bad", "localhost:0", "localhost:65536", "user@localhost:1025", "localhost/path:1025"} {
		in := base
		in.Address = address
		if _, e := normalizeRelay(in, true); e == nil {
			t.Errorf("accepted malformed SMTP address %q", address)
		}
	}
}
func TestNamedRelayRouting(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	save := func(in RelayInput) string {
		t.Helper()
		id, e := a.saveRelay(ctx, in)
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	relayA := RelayInput{Name: "Domain A", Address: "localhost:1025", Username: "relay-user", Password: "private-relay-password", TLS: "none", BounceDomain: "bounces.a.com", AllowedDomains: []string{"A.COM", "a.com"}, Enabled: true}
	idA := save(relayA)
	relayA.ID = idA
	relayA.Password = ""
	relayB := RelayInput{Name: "Domain B", Address: "localhost:1026", TLS: "none", AllowedDomains: []string{"b.com"}, Enabled: true}
	idB := save(relayB)
	relayB.ID = idB
	if e := a.setDomainRoute(ctx, "A.COM", idA); e != nil {
		t.Fatal(e)
	}
	if e := a.setDomainRoute(ctx, "b.com", idB); e != nil {
		t.Fatal(e)
	}
	for _, pair := range []struct{ from, id string }{{"Sender <sender@A.COM>", idA}, {"sender@b.com", idB}} {
		id, e := a.senderRelay(ctx, pair.from)
		if e != nil || id != pair.id {
			t.Fatalf("route %s: %q %v", pair.from, id, e)
		}
	}
	if id, e := a.senderRelay(ctx, "sender@sub.a.com"); e != nil || id != "" {
		t.Fatalf("exact-domain nonstrict route: %q %v", id, e)
	}
	if _, e := a.DB.Exec(ctx, "UPDATE platform_settings SET strict_sender_domains=true WHERE id=1"); e != nil {
		t.Fatal(e)
	}
	if _, e := a.senderRelay(ctx, "sender@sub.a.com"); e == nil {
		t.Fatal("strict mode admitted unmapped subdomain")
	}
	if _, e := a.senderRelay(ctx, "sender@other.com"); e == nil {
		t.Fatal("strict mode admitted unmapped domain")
	}
	if e := a.setDomainRoute(ctx, "a.com", idB); e == nil {
		t.Fatal("route accepted relay with wrong allowed domain")
	}
	c, e := a.relayConfig(ctx, idA, "sender@a.com", a.C)
	if e != nil || c.SMTPAddress != relayA.Address || c.SMTPUser != "relay-user" || c.SMTPPassword != "private-relay-password" || c.BounceDomain != "bounces.a.com" {
		t.Fatalf("relay configuration mismatch: %v", e)
	}
	if _, e = a.relayConfig(ctx, idA, "sender@b.com", a.C); e == nil {
		t.Fatal("relay config accepted wrong sender domain")
	}
	rows, e := a.listRelays(ctx)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := json.Marshal(rows)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(encoded), "private-relay-password") || strings.Contains(string(encoded), `"password":`) {
		t.Fatal("relay listing disclosed a password")
	}
	var encrypted string
	if e = a.DB.QueryRow(ctx, "SELECT password FROM smtp_relays WHERE id=$1", idA).Scan(&encrypted); e != nil {
		t.Fatal(e)
	}
	if encrypted == "" || encrypted == "private-relay-password" || strings.Contains(string(encoded), encrypted) {
		t.Fatal("relay secret is plaintext or disclosed")
	}
	save(relayA)
	c, e = a.relayConfig(ctx, idA, "sender@a.com", a.C)
	if e != nil || c.SMTPPassword != "private-relay-password" {
		t.Fatal("blank password did not retain saved secret")
	}
	relayA.ClearPassword = true
	save(relayA)
	c, e = a.relayConfig(ctx, idA, "sender@a.com", a.C)
	if e != nil || c.SMTPPassword != "" {
		t.Fatal("explicit clear did not remove secret")
	}
	relayA.ClearPassword = false
	relayA.Enabled = false
	save(relayA)
	if _, e = a.senderRelay(ctx, "sender@a.com"); e == nil {
		t.Fatal("disabled relay selected for admission")
	}
	if _, e = a.relayConfig(ctx, idA, "sender@a.com", a.C); e == nil {
		t.Fatal("disabled relay selected for dispatch")
	}
	relayA.Enabled = true
	save(relayA)
	relayA.AllowedDomains = nil
	save(relayA)
	if _, e = a.relayConfig(ctx, idA, "sender@a.com", a.C); e == nil {
		t.Fatal("empty allowed domain list allowed dispatch")
	}
	relayA.AllowedDomains = []string{"a.com"}
	save(relayA)
	relayA.FallbackID = idB
	save(relayA)
	if _, e = a.fallbackRelay(ctx, "sender@a.com", idA); e == nil {
		t.Fatal("fallback accepted target with wrong sender domain")
	}
	relayB.AllowedDomains = []string{"a.com", "b.com"}
	save(relayB)
	if id, e := a.fallbackRelay(ctx, "sender@a.com", idA); e != nil || id != idB {
		t.Fatalf("allowed fallback: %q %v", id, e)
	}
	relayB.FallbackID = idA
	if _, e = a.saveRelay(ctx, relayB); e == nil {
		t.Fatal("fallback cycle accepted")
	}
	relayB.FallbackID = ""
	relayA.FallbackID = idA
	if _, e = a.saveRelay(ctx, relayA); e == nil {
		t.Fatal("self fallback accepted")
	}
	relayA.FallbackID = idB
	relayB.Enabled = false
	save(relayB)
	if _, e = a.fallbackRelay(ctx, "sender@a.com", idA); e == nil {
		t.Fatal("disabled fallback accepted")
	}
	if e := a.setDomainRoute(ctx, "a.com", ""); e != nil {
		t.Fatal(e)
	}
	if _, e = a.senderRelay(ctx, "sender@a.com"); e == nil {
		t.Fatal("removed domain route admitted in strict mode")
	}
	if _, e = a.DB.Exec(ctx, "UPDATE platform_settings SET strict_sender_domains=false WHERE id=1"); e != nil {
		t.Fatal(e)
	}
	if id, e := a.senderRelay(ctx, "sender@a.com"); e != nil || id != "" {
		t.Fatalf("legacy routing was not restored: %q %v", id, e)
	}
}
func TestNamedRelayConnectionRetainsSecretWithoutSending(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	smtp := newRuntimeSMTPFixture(t, "", "relay-user", "relay-password")
	in := RelayInput{Name: "Test relay", Address: smtp.Address, Username: "relay-user", Password: "relay-password", TLS: "none", AllowedDomains: []string{"example.test"}, Enabled: true}
	id, e := a.saveRelay(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	in.ID = id
	in.Password = ""
	if e = a.testRelay(ctx, in); e != nil {
		t.Fatal(e)
	}
	if smtp.Noop.Load() != 1 || smtp.Auth.Load() != 1 || smtp.Data.Load() != 0 {
		t.Fatal("connection test did not authenticate and NOOP without sending")
	}
}
