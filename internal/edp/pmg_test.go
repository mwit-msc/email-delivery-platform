package edp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFailureClassification(t *testing.T) {
	cases := map[string]string{"550 5.7.1 Your IP is blocked by Spamhaus": "ip_reputation", "421 4.7.0 IP temporarily blocked": "temporary", "550 5.1.1 User unknown (blacklisted mailbox)": "invalid_recipient", "554 5.7.1 Malware detected": "content_policy", "554 5.7.1 Access denied": "other"}
	for detail, want := range cases {
		if got := classifyFailure(detail); got != want {
			t.Errorf("%s: %s != %s", detail, got, want)
		}
	}
	if pmgStatus(pmgRecord{DStatus: "B"}) != "" || pmgStatus(pmgRecord{DStatus: "Q"}) != "" {
		t.Fatal("security filtering became transport failure")
	}
}
func TestPMGTrackingAndSecondaryRouting(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	a.C.SecondarySMTPAddress = "secondary.example.test:587"
	a.DB.Exec(ctx, "UPDATE applications SET fallback_enabled=true WHERE id='app_demo'")
	submit := func(recipient string) string {
		id, _, e := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: recipient, Template: "welcome", Variables: map[string]string{"name": "Test"}}, recipient)
		if e != nil {
			t.Fatal(e)
		}
		a.DB.Exec(ctx, "UPDATE messages SET status='relay_accepted',relay_accepted_at=now() WHERE id=$1", id)
		return id
	}
	blocked := submit("blocked@example.test")
	deferred := submit("deferred@example.test")
	invalid := submit("invalid@example.test")
	delivered := submit("delivered@example.test")
	ids := map[string]string{"blocked@example.test": blocked, "deferred@example.test": deferred, "invalid@example.test": invalid, "delivered@example.test": delivered}
	records := map[string]pmgRecord{}
	for recipient, id := range ids {
		status := "5"
		if recipient == "deferred@example.test" {
			status = "4"
		}
		if recipient == "delivered@example.test" {
			status = "2"
		}
		records[recipient] = pmgRecord{ID: "Q" + id[4:12], MsgID: "<" + id + "@localhost>", From: "notifications@example.test", To: recipient, DStatus: "A", RStatus: status, Time: time.Now().Unix()}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api2/json/access/ticket", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("username") != "audit@pmg" || r.FormValue("password") != "test-password" {
			t.Error("wrong PMG login fields")
		}
		respond(w, 200, map[string]any{"data": map[string]string{"ticket": "test-ticket"}})
	})
	mux.HandleFunc("GET /api2/json/nodes/node1/tracker", func(w http.ResponseWriter, r *http.Request) {
		c, e := r.Cookie("PMGAuthCookie")
		if e != nil || c.Value != "test-ticket" {
			problem(w, 401, "ticket missing")
			return
		}
		record := records[r.URL.Query().Get("target")]
		respond(w, 200, map[string]any{"data": []pmgRecord{record}})
	})
	mux.HandleFunc("GET /api2/json/nodes/node1/tracker/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, record := range records {
			if record.ID == r.PathValue("id") {
				reply := "550 5.7.1 Client IP blocked by Spamhaus"
				if record.To == "invalid@example.test" {
					reply = "550 5.1.1 User unknown"
				}
				record.Logs = []string{fmt.Sprintf("postfix/smtp[99]: Q: to=<%s>, relay=remote[1.2.3.4]:25, dsn=5.7.1, status=bounced (host said: %s)", record.To, reply)}
				respond(w, 200, map[string]any{"data": record})
				return
			}
		}
		problem(w, 404, "unknown queue")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	a.C.PMGURL = server.URL
	a.C.PMGUser = "audit@pmg"
	a.C.PMGPassword = "test-password"
	a.C.PMGNodes = []string{"node1"}
	if e := a.PollPMG(ctx); e != nil {
		t.Fatal(e)
	}
	check := func(id, status, route string) {
		var got, gotRoute string
		a.DB.QueryRow(ctx, "SELECT status,route FROM messages WHERE id=$1", id).Scan(&got, &gotRoute)
		if got != status || gotRoute != route {
			t.Fatalf("%s: status %s route %s", id, got, gotRoute)
		}
	}
	check(blocked, "queued", "secondary")
	check(deferred, "relay_accepted", "primary")
	check(invalid, "bounced", "primary")
	check(delivered, "remote_accepted", "primary")
	if e := a.PollPMG(ctx); e != nil {
		t.Fatal(e)
	}
	var n int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM delivery_events WHERE message_id=$1 AND status='fallback_queued'", blocked).Scan(&n)
	if n != 1 {
		t.Fatal("fallback duplicated", n)
	}
	// A failed secondary must not produce a third delivery route.
	a.DB.Exec(ctx, "UPDATE messages SET status='failed',failure_category='ip_reputation' WHERE id=$1", blocked)
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.queueFallback(ctx, tx, blocked); e != nil {
		t.Fatal(e)
	}
	tx.Commit(ctx)
	check(blocked, "failed", "secondary")
	// Other-recipient, temporary, and quarantine evidence cannot authorize fallback.
	record := records["blocked@example.test"]
	record.Logs = []string{"postfix/smtp[99]: Q: to=<other@example.test>, dsn=5.7.1, status=bounced (550 IP blocked)"}
	_, category := pmgBounceDetail(record, record.To)
	if category == "ip_reputation" {
		t.Fatal("uncorrelated recipient triggered fallback")
	}
	record.MsgID = "<unrelated@localhost>"
	if correlatePMGRecord(record, blocked, record.To, record.From) {
		t.Fatal("wrong message ID matched")
	}
}
func TestDirectSMTPRejectionRoutesOnce(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	a.C.SecondarySMTPAddress = "secondary.example.test:587"
	a.DB.Exec(ctx, "UPDATE applications SET fallback_enabled=true WHERE id='app_demo'")
	_, _, e := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "smtp@example.test", Template: "welcome", Variables: map[string]string{"name": "Test"}}, "smtp-blocked")
	if e != nil {
		t.Fatal(e)
	}
	d, e := a.claim(ctx)
	if e != nil || d == nil {
		t.Fatal(e)
	}
	if e = a.finish(ctx, *d, "failed", "SMTP 550: 5.7.1 Client IP is blocked by Spamhaus"); e != nil {
		t.Fatal(e)
	}
	var route, status string
	a.DB.QueryRow(ctx, "SELECT route,status FROM messages WHERE id=$1", d.ID).Scan(&route, &status)
	if route != "secondary" || status != "queued" {
		t.Fatal("confirmed primary rejection did not route secondary")
	}
}
