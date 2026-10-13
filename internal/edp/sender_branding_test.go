package edp

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSenderDisplayNameMIME(t *testing.T) {
	for _, name := range []string{"Events team", "คณะกรรมการนักเรียน", strings.Repeat("ก", 120), `Events "desk", certificates`, ""} {
		raw := makeMIME(delivery{ID: "synthetic", From: "sender@example.test", FromName: name, Recipient: "to@example.test", Subject: "Subject"}, "https://edp.example.test")
		message, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		from, err := mail.ParseAddress(message.Header.Get("From"))
		if err != nil || from.Address != "sender@example.test" || from.Name != name {
			t.Fatal("display name did not round-trip", err, from)
		}
		for _, line := range strings.Split(string(raw), "\r\n") {
			if len(line) > 998 {
				t.Fatal("MIME exceeded header line limit")
			}
		}
	}
	for _, name := range []string{"bad\r\nBcc: another@example.test", "bad\x00", strings.Repeat("ก", 121)} {
		if validDisplayName(name) {
			t.Fatal("unsafe name accepted")
		}
	}
}

func TestSenderDisplayNameSettingsAndFrozenJobs(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	session := runtimeTestSession(t, a, true)
	fields := url.Values{"csrf": {"runtime-csrf"}, "action": {"settings"}, "name": {"Demo"}, "from": {"notifications@example.test"}, "from_name": {"คณะกรรมการนักเรียน"}, "daily": {"5000"}, "rate": {"60"}, "recipient": {"100"}, "queue": {"5000"}}
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/applications/app_demo", strings.NewReader(fields.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		return w
	}
	if w := request(); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	id, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "one@example.test", Template: "welcome", Variables: map[string]string{"name": "One"}}, "named-sender")
	if err != nil {
		t.Fatal(err)
	}
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	cid := newID("cmp_")
	if _, err = a.DB.Exec(ctx, "INSERT INTO campaigns(id,application_id,name,kind,created_by,source_rows) VALUES($1,'app_demo','Synthetic','bulk',$2,'[[\"email\",\"name\"],[\"campaign@example.test\",\"Campaign\"]]')", cid, root); err != nil {
		t.Fatal(err)
	}
	if err = a.mapBulkCampaign(ctx, cid, "tpl_welcome", 0, map[string]int{"name": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.approveCampaign(ctx, cid, root); err != nil {
		t.Fatal(err)
	}
	if err = a.startCampaign(ctx, cid, time.Now()); err != nil {
		t.Fatal(err)
	}
	fields.Set("from_name", "New display name")
	if w := request(); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var count int
	if err = a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE (id=$1 OR campaign_id=$2) AND from_name=$3", id, cid, "คณะกรรมการนักเรียน").Scan(&count); err != nil || count != 2 {
		t.Fatal("queued sender name was not frozen", count, err)
	}
	d, err := a.claim(ctx)
	if err != nil || d == nil || d.FromName != "คณะกรรมการนักเรียน" {
		t.Fatal("worker lost frozen display name", err)
	}
	fields.Set("from_name", "bad\r\nBcc: injected@example.test")
	if w := request(); w.Code != http.StatusBadRequest {
		t.Fatal("header injection accepted", w.Code)
	}
}

func TestInboxBrandingDNS(t *testing.T) {
	records := map[string][]string{"_dmarc.example.test": {"v=DMARC1; p=reject; pct=100"}, "default._bimi.mail.example.test": {"v=BIMI1; l=https://assets.example.test/logo.svg; a=https://assets.example.test/cert.pem"}}
	lookup := func(ctx context.Context, name string) ([]string, error) {
		if values, ok := records[name]; ok {
			return values, nil
		}
		return nil, &net.DNSError{IsNotFound: true, Name: name, Err: "no record"}
	}
	checks := checkInboxBranding(context.Background(), "sender@mail.example.test", lookup)
	if checks[0].Status != "Enforced policy published" || checks[1].Status != "URLs published" {
		t.Fatal(checks)
	}
	records["_dmarc.example.test"] = []string{"v=DMARC1; p=reject; sp=none"}
	checks = checkInboxBranding(context.Background(), "sender@mail.example.test", lookup)
	if checks[0].Status == "Enforced policy published" {
		t.Fatal("ignored inherited subdomain policy")
	}
	records["default._bimi.mail.example.test"] = []string{"v=BIMI1; l=https://assets.example.test/logo.svg; a=http://assets.example.test/cert.pem"}
	checks = checkInboxBranding(context.Background(), "sender@mail.example.test", lookup)
	if checks[1].Status == "URLs published" {
		t.Fatal("insecure certificate URL accepted")
	}
	records["default._bimi.mail.example.test"] = []string{"v=BIMI1; l=https://assets.example.test/one.svg", "v=BIMI1; l=https://assets.example.test/two.svg"}
	checks = checkInboxBranding(context.Background(), "sender@mail.example.test", lookup)
	if !strings.Contains(checks[1].Detail, "Multiple") {
		t.Fatal("ambiguous BIMI policy accepted")
	}
}
