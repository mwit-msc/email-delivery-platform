package edp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPasswordAndRendering(t *testing.T) {
	hash, e := hashPassword("correct horse battery staple")
	if e != nil || !checkPassword(hash, "correct horse battery staple") || checkPassword(hash, "wrong") {
		t.Fatal("password verification")
	}
	if validPasswordHash("$argon2id$v=19$m=999999999,t=2,p=1$bad$bad") {
		t.Fatal("unbounded hashing parameters")
	}
	rendered, e := renderVariables("Hello {{name}}", map[string]string{"name": "<script>"}, true)
	if e != nil || rendered != "Hello &lt;script&gt;" {
		t.Fatalf("escaped variables: %q %v", rendered, e)
	}
	if _, e = renderVariables("{{certificate_url}}", map[string]string{"certificate_url": "javascript:alert(1)"}, true); e == nil {
		t.Fatal("unsafe link accepted")
	}
	s := cleanHTML(`<script>alert(1)</script><p onclick="alert(1)" style="color:red">Hello</p><a href="javascript:alert(1)">bad</a>`)
	if strings.Contains(s, "script") || strings.Contains(s, "onclick") {
		t.Fatalf("unsafe HTML survived %s", s)
	}
	if !strings.Contains(s, "Hello") {
		t.Fatal("lost email content")
	}
}
func testApp(t *testing.T) *App {
	t.Helper()
	raw := os.Getenv("EDP_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("EDP_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, e := pgxpool.New(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	schema := newID("edp_test_")
	if _, e = pool.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	app, e := New(ctx, Config{DatabaseURL: u.String(), Dev: true, PublicURL: "http://localhost:8080", RootUsername: "root", RootPassword: "test-root-password", Secret: make([]byte, 32), SMTPAddress: "localhost:1025", SMTPTLS: "none", RetentionDays: 30, SeedCertificatesFile: "testdata/certificates.json"}, "test")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { app.Close(); pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); pool.Close() })
	if e = app.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = app.Seed(ctx, true); e != nil {
		t.Fatal(e)
	}
	// Released-version regression fixtures retain the legacy compatibility branch.
	// Named-only upgrade tests explicitly restore the migration default.
	if _, e = app.DB.Exec(ctx, "UPDATE platform_settings SET named_relays_only=false,strict_sender_domains=false; DELETE FROM sender_domain_routes; DELETE FROM smtp_relays"); e != nil {
		t.Fatal(e)
	}
	return app
}
func TestPlatformIntegration(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var rootID string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&rootID)
	root := &Principal{ID: rootID, Root: true, Method: "local", CSRF: "csrf"}
	t.Run("seeding preserves root", func(t *testing.T) {
		a.C.RootPassword = "different-root-password"
		if e := a.Seed(ctx, true); e != nil {
			t.Fatal(e)
		}
		var n int
		var hash string
		a.DB.QueryRow(ctx, "SELECT count(*) FROM users WHERE root").Scan(&n)
		a.DB.QueryRow(ctx, "SELECT password_hash FROM users WHERE root").Scan(&hash)
		if n != 1 || !checkPassword(hash, "test-root-password") {
			t.Fatal("seed reset or duplicated root")
		}
		a.C.RootPassword = "test-root-password"
		if e := a.Migrate(ctx); e != nil {
			t.Fatal("migration is not idempotent", e)
		}
	})
	t.Run("local login and CSRF", func(t *testing.T) {
		handler := a.Handler()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest("GET", "/login", nil))
		if res.Code != 200 {
			t.Fatalf("login page: %d %s", res.Code, res.Body)
		}
		var csrf *http.Cookie
		for _, c := range res.Result().Cookies() {
			if c.Name == "edp_login_csrf" {
				csrf = c
			}
		}
		if csrf == nil {
			t.Fatal("login CSRF missing")
		}
		form := url.Values{"username": {"root"}, "password": {"test-root-password"}, "csrf": {csrf.Value}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrf)
		res = httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != 303 {
			t.Fatalf("local login failed: %d %s", res.Code, res.Body)
		}
		var session *http.Cookie
		for _, c := range res.Result().Cookies() {
			if c.Name == "edp_session" {
				session = c
			}
		}
		if session == nil {
			t.Fatal("session missing")
		}
		for _, path := range []string{"/", "/applications", "/applications/app_demo", "/templates?app=app_demo", "/templates/tpl_welcome", "/campaigns?app=app_demo", "/access", "/settings", "/audit", "/statistics", "/metrics"} {
			req = httptest.NewRequest("GET", path, nil)
			req.AddCookie(session)
			res = httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != 200 {
				t.Errorf("%s: %d %s", path, res.Code, res.Body)
			}
		}
		req = httptest.NewRequest("POST", "/settings", strings.NewReader("rate=100&connections=2"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		res = httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != 403 {
			t.Fatal("missing CSRF accepted")
		}
	})
	t.Run("scope isolation and fresh groups", func(t *testing.T) {
		a.DB.Exec(ctx, "INSERT INTO users(id,username,name) VALUES('viewer','viewer','Viewer')")
		a.DB.Exec(ctx, "INSERT INTO bindings(user_id,scope,role) VALUES('viewer','app_demo','viewer')")
		p := &Principal{ID: "viewer", Method: "local"}
		if !a.Allowed(ctx, p, "app_demo", "message.view") || a.Allowed(ctx, p, "other", "message.view") || a.Allowed(ctx, p, "app_demo", "message.send") {
			t.Fatal("RBAC escaped scope")
		}
		a.DB.Exec(ctx, "INSERT INTO group_mappings(group_slug,scope,role) VALUES('organizers','app_demo','operator')")
		p.Groups = []string{"organizers"}
		p.GroupsFresh = true
		if a.Allowed(ctx, p, "app_demo", "campaign.send") {
			t.Fatal("local login reused SSO groups")
		}
		p.Method = "sso"
		if !a.Allowed(ctx, p, "app_demo", "campaign.send") {
			t.Fatal("fresh group role missing")
		}
		p.GroupsFresh = false
		if a.Allowed(ctx, p, "app_demo", "campaign.send") {
			t.Fatal("stale groups granted access")
		}
		if !a.Allowed(ctx, root, "*", "platform.manage") {
			t.Fatal("root inaccessible")
		}
	})
	t.Run("idempotency and isolation", func(t *testing.T) {
		in := SendInput{ApplicationID: "app_demo", To: "one@example.test", Template: "welcome", Variables: map[string]string{"name": "Person"}}
		id, replayed, e := a.Submit(ctx, in, "same-key")
		if e != nil || replayed {
			t.Fatal(id, replayed, e)
		}
		second, replayed, e := a.Submit(ctx, in, "same-key")
		if e != nil || !replayed || second != id {
			t.Fatal("retry duplicated message")
		}
		in.Variables["name"] = "Other"
		if _, _, e = a.Submit(ctx, in, "same-key"); e == nil {
			t.Fatal("conflicting payload accepted")
		}
		_, e = a.DB.Exec(ctx, "INSERT INTO api_keys(id,application_id,name,token_hash,scopes) VALUES('test-key','app_demo','Test',$1,ARRAY['message.view','message.send'])", tokenHash("test-token"))
		if e != nil {
			t.Fatal(e)
		}
		handler := a.Handler()
		req := httptest.NewRequest("GET", "/v1/messages/"+id, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != 200 {
			t.Fatal(res.Code, res.Body)
		}
		a.DB.Exec(ctx, "UPDATE api_keys SET revoked=true WHERE id='test-key'")
		res = httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != 401 {
			t.Fatal("revoked key accepted", res.Code)
		}
	})
	t.Run("three thousand submissions respect admission quota", func(t *testing.T) {
		a.DB.Exec(ctx, "UPDATE applications SET daily_limit=25,queue_limit=25,recipient_hour_limit=100 WHERE id='app_demo'")
		a.DB.Exec(ctx, "DELETE FROM messages")
		var accepted atomic.Int64
		var unexpected atomic.Int64
		jobs := make(chan int)
		var wg sync.WaitGroup
		for n := 0; n < 12; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					_, _, e := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: fmt.Sprintf("p%d@example.test", i), Template: "welcome", Variables: map[string]string{"name": "Person"}}, fmt.Sprintf("burst-%d", i))
					if e == nil {
						accepted.Add(1)
					} else if h, ok := e.(*HTTPError); !ok || h.Code != 429 {
						unexpected.Add(1)
					}
				}
			}()
		}
		for i := 0; i < 3000; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		if accepted.Load() != 25 || unexpected.Load() != 0 {
			t.Fatalf("accepted=%d unexpected=%d", accepted.Load(), unexpected.Load())
		}
		var stored int
		a.DB.QueryRow(ctx, "SELECT count(*) FROM messages").Scan(&stored)
		if stored != 25 {
			t.Fatal("queue exceeded quota", stored)
		}
		a.DB.Exec(ctx, "UPDATE applications SET daily_limit=5000,queue_limit=5000 WHERE id='app_demo'")
	})
	t.Run("worker rate limit and uncertain crash", func(t *testing.T) {
		d, e := a.claim(ctx)
		if e != nil || d == nil {
			t.Fatal("claim", e)
		}
		next, e := a.claim(ctx)
		if e != nil || next != nil {
			t.Fatal("global rate limit not shared")
		}
		a.DB.Exec(ctx, "UPDATE messages SET lease_until=now()-interval '1 second' WHERE id=$1", d.ID)
		a.maintenance(ctx)
		var state string
		a.DB.QueryRow(ctx, "SELECT status FROM messages WHERE id=$1", d.ID).Scan(&state)
		if state != "unknown" {
			t.Fatal("crashed send was blindly retried", state)
		}
		if e = a.finish(ctx, *d, "relay_accepted", ""); e == nil {
			t.Fatal("stale worker overwrote uncertain state")
		}
		a.DB.Exec(ctx, "UPDATE messages SET expires_at=now()-interval '1 second' WHERE status='queued'")
		a.maintenance(ctx)
		var n int
		a.DB.QueryRow(ctx, "SELECT count(*) FROM messages WHERE status='expired'").Scan(&n)
		if n != 24 {
			t.Fatal("expired jobs remained sendable", n)
		}
	})
	t.Run("campaign approval and resume", func(t *testing.T) {
		a.DB.Exec(ctx, "INSERT INTO campaigns(id,application_id,name,created_by,source_rows) VALUES('campaign-test','app_demo','Certificates',$1,$2)", rootID, []byte(`[["email","school","link"],["school@example.test","Example School","https://drive.google.com/example"]]`))
		if e := a.mapCampaign(ctx, "campaign-test", Mapping{Email: 0, School: 1, Event: -1, Award: -1, URL: 2, DefaultEvent: "square", DefaultAward: "participation"}); e != nil {
			t.Fatal(e)
		}
		if e := a.approveCampaign(ctx, "campaign-test", rootID); e != nil {
			t.Fatal(e)
		}
		if e := a.mapCampaign(ctx, "campaign-test", Mapping{}); e == nil {
			t.Fatal("approved campaign mutated")
		}
		if e := a.startCampaign(ctx, "campaign-test", time.Now()); e != nil {
			t.Fatal(e)
		}
		if e := a.startCampaign(ctx, "campaign-test", time.Now()); e == nil {
			t.Fatal("campaign started twice")
		}
		a.DB.Exec(ctx, "UPDATE campaigns SET state='paused' WHERE id='campaign-test'")
		a.DB.Exec(ctx, "UPDATE platform_settings SET next_send_at=now() WHERE id=1")
		d, e := a.claim(ctx)
		if e != nil || d != nil {
			t.Fatal("paused campaign claimed")
		}
		a.DB.Exec(ctx, "UPDATE campaigns SET state='sending' WHERE id='campaign-test'")
		a.DB.Exec(ctx, "UPDATE applications SET next_send_at=now() WHERE id='app_demo'")
		d, e = a.claim(ctx)
		if e != nil || d == nil {
			t.Fatal("resume did not release queued message", e)
		}
		if e = a.finish(ctx, *d, "relay_accepted", "test"); e != nil {
			t.Fatal(e)
		}
		a.maintenance(ctx)
		var state string
		a.DB.QueryRow(ctx, "SELECT state FROM campaigns WHERE id='campaign-test'").Scan(&state)
		if state != "completed" {
			t.Fatal("campaign never completed")
		}
	})
	t.Run("versioned visual editor saves immutable snapshot", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/templates/tpl_welcome/data", strings.NewReader(`{"Subject":"New {{name}}","HTML":"<p>{{name}}</p><script>alert(1)</script>","Plain":"Hello {{name}}","Project":{"pages":[]},"Version":1}`))
		r.Header.Set("X-CSRF-Token", "csrf")
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, root))
		r.SetPathValue("id", "tpl_welcome")
		w := httptest.NewRecorder()
		a.editorData(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		var reply map[string]any
		json.Unmarshal(w.Body.Bytes(), &reply)
		old, e := a.renderMessage(ctx, "app_demo", "welcome", 1, map[string]string{"name": "Person"})
		if e != nil || old.Subject != "Welcome, Person" {
			t.Fatal("old version changed")
		}
		current, e := a.renderMessage(ctx, "app_demo", "welcome", 0, map[string]string{"name": "Person"})
		if e != nil || current.Subject != "New Person" || strings.Contains(current.HTML, "script") {
			t.Fatal("current sanitized version missing")
		}
	})
}

func TestCampaignMutationsRemainFrozen(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var uid string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&uid)
	a.DB.Exec(ctx, "INSERT INTO campaigns(id,application_id,name,created_by) VALUES('frozen','app_demo','Frozen',$1)", uid)
	rows := [][]string{{"email", "school", "link"}, {"school@example.test", "School", "https://drive.google.com/example"}}
	if e := a.replaceCampaignSource(ctx, "frozen", rows); e != nil {
		t.Fatal(e)
	}
	if e := a.mapCampaign(ctx, "frozen", Mapping{Email: 0, School: 1, Event: -1, Award: -1, URL: 2, DefaultEvent: "square", DefaultAward: "participation"}); e != nil {
		t.Fatal(e)
	}
	var rowID string
	a.DB.QueryRow(ctx, "SELECT id FROM campaign_rows WHERE campaign_id='frozen'").Scan(&rowID)
	if e := a.approveCampaign(ctx, "frozen", uid); e != nil {
		t.Fatal(e)
	}
	if e := a.replaceCampaignSource(ctx, "frozen", rows); e == nil {
		t.Fatal("approved import accepted")
	}
	if e := a.toggleCampaignRow(ctx, "frozen", rowID); e == nil {
		t.Fatal("approved exclusion accepted")
	}
	var n int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM campaign_rows WHERE campaign_id='frozen' AND NOT excluded").Scan(&n)
	if n != 1 {
		t.Fatal("approved recipients were changed")
	}
}
func TestDeliveryFeedbackRequiresAuth(t *testing.T) {
	a := testApp(t)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/delivery-events", strings.NewReader(`{}`)))
	if w.Code != 401 {
		t.Fatal("unauthenticated feedback was acknowledged", w.Code)
	}
}
func TestCSVRejectsOversize(t *testing.T) {
	_, e := parseCSV(strings.NewReader("email,school\n" + strings.Repeat("a", (8<<20)+1)))
	if e == nil {
		t.Fatal("oversized CSV silently truncated")
	}
}

func TestApplicationAdminCannotRaiseOwnQuota(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	a.DB.Exec(ctx, "INSERT INTO users(id,username,name) VALUES('app-admin','app-admin','App admin')")
	a.DB.Exec(ctx, "INSERT INTO bindings(user_id,scope,role) VALUES('app-admin','app_demo','admin')")
	p := &Principal{ID: "app-admin", Method: "local", CSRF: "csrf"}
	form := url.Values{"csrf": {"csrf"}, "action": {"settings"}, "name": {"Demo application"}, "from": {"notifications@example.test"}, "daily": {"999999"}, "rate": {"60"}, "recipient": {"20"}, "queue": {"5000"}}
	r := httptest.NewRequest("POST", "/applications/app_demo", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
	r.SetPathValue("id", "app_demo")
	w := httptest.NewRecorder()
	a.application(w, r)
	if w.Code != 403 {
		t.Fatal("application administrator changed quota", w.Code)
	}
	var daily int
	a.DB.QueryRow(ctx, "SELECT daily_limit FROM applications WHERE id='app_demo'").Scan(&daily)
	if daily != 5000 {
		t.Fatal("quota changed")
	}
}
func TestDeliveryFeedbackDeduplicatesAndScopes(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	id, _, e := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "feedback@example.test", Template: "welcome", Variables: map[string]string{"name": "Test"}}, "feedback-message")
	if e != nil {
		t.Fatal(e)
	}
	a.DB.Exec(ctx, "UPDATE messages SET status='relay_accepted',relay_accepted_at=now() WHERE id=$1", id)
	a.DB.Exec(ctx, "INSERT INTO api_keys(id,application_id,name,token_hash,scopes) VALUES('report-key','app_demo','Reporting',$1,ARRAY['delivery.report'])", tokenHash("report-token"))
	request := func(event, status string) int {
		body, _ := json.Marshal(map[string]string{"message_id": id, "status": status, "event_key": event})
		r := httptest.NewRequest("POST", "/v1/delivery-events", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer report-token")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if request("accepted", "remote_accepted") != 200 || request("accepted", "remote_accepted") != 200 {
		t.Fatal("feedback replay failed")
	}
	if request("accepted", "bounced") != 409 {
		t.Fatal("event key collision accepted")
	}
	if request("bounce", "bounced") != 200 {
		t.Fatal("bounce missing")
	}
	var n int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM delivery_events WHERE event_key IS NOT NULL").Scan(&n)
	if n != 2 {
		t.Fatal("feedback duplicated", n)
	}
}

func TestEditorProjectCannotRestoreScripts(t *testing.T) {
	raw, e := cleanProject(json.RawMessage(`{"pages":[{"component":{"type":"text","script":"fetch('/access')","attributes":{"onclick":"alert(1)","href":"javascript:alert(1)"},"content":"<p>Safe</p><script>alert(1)</script>"}}]}`))
	if e != nil || strings.Contains(string(raw), "onclick") || strings.Contains(string(raw), "javascript:") || strings.Contains(string(raw), "fetch") {
		t.Fatal("project restored executable content", string(raw), e)
	}
}
func TestGenericBulkCampaignMapsTemplateVariables(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var uid string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&uid)
	a.DB.Exec(ctx, "INSERT INTO campaigns(id,application_id,name,created_by,kind) VALUES('bulk-test','app_demo','Bulk',$1,'bulk')", uid)
	rows := [][]string{{"email", "name"}, {"person@example.test", "Person"}}
	if e := a.replaceCampaignSource(ctx, "bulk-test", rows); e != nil {
		t.Fatal(e)
	}
	if e := a.mapBulkCampaign(ctx, "bulk-test", "tpl_welcome", 0, map[string]int{"name": 1}, nil); e != nil {
		t.Fatal(e)
	}
	if e := a.approveCampaign(ctx, "bulk-test", uid); e != nil {
		t.Fatal(e)
	}
	if e := a.startCampaign(ctx, "bulk-test", time.Now()); e != nil {
		t.Fatal(e)
	}
	var subject string
	a.DB.QueryRow(ctx, "SELECT subject FROM messages WHERE campaign_id='bulk-test'").Scan(&subject)
	if subject != "Welcome, Person" {
		t.Fatal("bulk personalization failed", subject)
	}
}
func TestSidebarMatchesWorkspaceContext(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var uid string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&uid)
	p := &Principal{ID: uid, Name: "Root", Root: true, Method: "local", CSRF: "csrf"}
	r := httptest.NewRequest("GET", "/campaigns?app=app_demo", nil)
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
	w := httptest.NewRecorder()
	a.campaigns(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	text := w.Body.String()
	if strings.Contains(text, "People and access") || strings.Contains(text, "Platform settings") || !strings.Contains(text, `href="/campaigns?app=app_demo" aria-current="page"`) {
		t.Fatal("app workspace showed global admin or missing active page")
	}
	r = httptest.NewRequest("GET", "/settings", nil)
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
	w = httptest.NewRecorder()
	a.settings(w, r)
	if !strings.Contains(w.Body.String(), `href="/settings" aria-current="page"`) || !strings.Contains(w.Body.String(), "People and access") {
		t.Fatal("global navigation missing")
	}
}
