package edp

import (
	"context"
	"strings"
	"testing"
)

func relayDeliveryClaim(t *testing.T, a *App) *delivery {
	t.Helper()
	ctx := context.Background()
	if _, e := a.DB.Exec(ctx, "UPDATE platform_settings SET next_send_at=now(); UPDATE applications SET next_send_at=now()"); e != nil {
		t.Fatal(e)
	}
	d, e := a.claim(ctx)
	if e != nil || d == nil {
		t.Fatal("missing dispatch", e)
	}
	return d
}
func TestNamedRelayActualSMTPRouting(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	x := newRuntimeSMTPFixture(t, "", "x-user", "x-secret")
	y := newRuntimeSMTPFixture(t, "", "y-user", "y-secret")
	rx, e := a.saveRelay(ctx, RelayInput{Name: "X", Address: x.Address, Username: "x-user", Password: "x-secret", TLS: "none", AllowedDomains: []string{"a.com"}, Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	ry, e := a.saveRelay(ctx, RelayInput{Name: "Y", Address: y.Address, Username: "y-user", Password: "y-secret", TLS: "none", AllowedDomains: []string{"a.com", "b.com"}, Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	for domain, id := range map[string]string{"a.com": rx, "b.com": ry} {
		if e = a.setDomainRoute(ctx, domain, id); e != nil {
			t.Fatal(e)
		}
	}
	a.DB.Exec(ctx, "UPDATE platform_settings SET strict_sender_domains=true")
	for n, domain := range []string{"a.com", "b.com"} {
		a.DB.Exec(ctx, "UPDATE applications SET from_address=$1 WHERE id='app_demo'", "notifications@"+domain)
		id, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "synthetic@example.test", Template: "welcome", Variables: map[string]string{"name": "Synthetic"}}, "named-route-"+domain)
		if err != nil {
			t.Fatal(err)
		}
		d := relayDeliveryClaim(t, a)
		if d.ID != id {
			t.Fatal("wrong message claimed")
		}
		status, detail := a.smtpSend(ctx, *d)
		if status != "relay_accepted" {
			t.Fatal(status, detail)
		}
		if err = a.finish(ctx, *d, status, detail); err != nil {
			t.Fatal(err)
		}
		if x.Data.Load() != 1 || y.Data.Load() != int64(n) {
			t.Fatal("wrong relay submitted DATA", x.Data.Load(), y.Data.Load())
		}
	}
	a.DB.Exec(ctx, "UPDATE applications SET from_address='notifications@unmapped.com' WHERE id='app_demo'")
	if _, _, e = a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "other@example.test", Template: "welcome", Variables: map[string]string{"name": "Synthetic"}}, "unmapped"); e == nil {
		t.Fatal("strict admission accepted an unmapped sender")
	}
	// Routing updates before dispatch must replace the old admission selection.
	a.DB.Exec(ctx, "UPDATE applications SET from_address='notifications@a.com' WHERE id='app_demo'")
	id, _, e := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "updated@example.test", Template: "welcome", Variables: map[string]string{"name": "Synthetic"}}, "route-updated")
	if e != nil {
		t.Fatal(e)
	}
	if e = a.setDomainRoute(ctx, "a.com", ry); e != nil {
		t.Fatal(e)
	}
	d := relayDeliveryClaim(t, a)
	status, detail := a.smtpSend(ctx, *d)
	if status != "relay_accepted" {
		t.Fatal(status, detail)
	}
	var selected string
	if e = a.DB.QueryRow(ctx, "SELECT relay_id FROM messages WHERE id=$1", id).Scan(&selected); e != nil || selected != ry || x.Data.Load() != 1 || y.Data.Load() != 2 {
		t.Fatal("stale route used", selected, e)
	}
}

func TestNamedRelayFallbackAndRevocation(t *testing.T) {
	for _, scenario := range []string{"one fallback", "revoked application", "reassigned source route"} {
		t.Run(scenario, func(t *testing.T) {
			revoke := scenario != "one fallback"
			a := testApp(t)
			ctx := context.Background()
			x := newRuntimeSMTPFixture(t, "550 5.7.1 IP blocked by Spamhaus", "x-user", "x-secret")
			y := newRuntimeSMTPFixture(t, "", "y-user", "y-secret")
			ry, e := a.saveRelay(ctx, RelayInput{Name: "Y", Address: y.Address, Username: "y-user", Password: "y-secret", TLS: "none", AllowedDomains: []string{"a.com"}, Enabled: true})
			if e != nil {
				t.Fatal(e)
			}
			rx, e := a.saveRelay(ctx, RelayInput{Name: "X", Address: x.Address, Username: "x-user", Password: "x-secret", TLS: "none", AllowedDomains: []string{"a.com"}, Enabled: true, FallbackID: ry})
			if e != nil {
				t.Fatal(e)
			}
			if e = a.setDomainRoute(ctx, "a.com", rx); e != nil {
				t.Fatal(e)
			}
			a.DB.Exec(ctx, "UPDATE platform_settings SET strict_sender_domains=true;UPDATE applications SET from_address='notifications@a.com',fallback_enabled=true WHERE id='app_demo'")
			id, _, e := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "synthetic@example.test", Template: "welcome", Variables: map[string]string{"name": "Synthetic"}}, "named-fallback")
			if e != nil {
				t.Fatal(e)
			}
			first := relayDeliveryClaim(t, a)
			status, detail := a.smtpSend(ctx, *first)
			if status != "failed" || !strings.Contains(detail, "Spamhaus") || x.Data.Load() != 0 {
				t.Fatal(status, detail)
			}
			if e = a.finish(ctx, *first, status, detail); e != nil {
				t.Fatal(e)
			}
			next := relayDeliveryClaim(t, a)
			if next.ID != id || next.Route != "secondary" || next.FallbackRelayID != ry {
				t.Fatal("missing named fallback", next)
			}
			if scenario == "revoked application" {
				a.DB.Exec(ctx, "UPDATE applications SET fallback_enabled=false WHERE id='app_demo'")
			}
			if scenario == "reassigned source route" {
				if err := a.setDomainRoute(ctx, "a.com", ry); err != nil {
					t.Fatal(err)
				}
			}
			status, detail = a.smtpSend(ctx, *next)
			if revoke {
				if status == "relay_accepted" || y.Data.Load() != 0 {
					t.Fatal("revoked fallback submitted DATA", status)
				}
			} else {
				if status != "relay_accepted" || y.Data.Load() != 1 {
					t.Fatal(status, detail)
				}
				if e = a.finish(ctx, *next, status, detail); e != nil {
					t.Fatal(e)
				}
				var events int
				a.DB.QueryRow(ctx, "SELECT count(*) FROM delivery_events WHERE message_id=$1 AND status='fallback_queued'", id).Scan(&events)
				if events != 1 {
					t.Fatal("fallback was not bounded to one", events)
				}
			}
		})
	}
}
