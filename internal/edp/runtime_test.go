package edp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeTestSession(t *testing.T, a *App, root bool) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	var id string
	if root {
		if err := a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&id); err != nil {
			t.Fatal(err)
		}
	} else {
		id = newID("usr_")
		if _, err := a.DB.Exec(ctx, "INSERT INTO users(id,username,name) VALUES($1,$1,'Application administrator')", id); err != nil {
			t.Fatal(err)
		}
		if _, err := a.DB.Exec(ctx, "INSERT INTO bindings(user_id,scope,role) VALUES($1,'app_demo','admin')", id); err != nil {
			t.Fatal(err)
		}
	}
	token := newID("session_")
	if _, err := a.DB.Exec(ctx, "INSERT INTO sessions(token_hash,user_id,method,csrf,expires_at) VALUES($1,$2,'local','runtime-csrf',now()+interval '1 hour')", tokenHash(token), id); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "edp_session", Value: token}
}

func runtimeSettingsRequest(a *App, session *http.Cookie, fields url.Values, csrf bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(fields.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if csrf {
		req.Header.Set("X-CSRF-Token", "runtime-csrf")
	}
	req.AddCookie(session)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, req)
	return response
}

func TestRuntimeSettingsSecretsAndPermissions(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	a.C.SMTPAddress, a.C.SMTPUser, a.C.SMTPPassword = "", "", ""
	a.C.SecondarySMTPAddress, a.C.SecondarySMTPPassword = "", ""
	a.C.PMGURL, a.C.PMGUser, a.C.PMGPassword = "", "", ""
	a.C.Issuer, a.C.ClientID, a.C.ClientSecret = "", "", ""
	root := runtimeTestSession(t, a, true)
	appAdmin := runtimeTestSession(t, a, false)
	smtp := url.Values{"action": {"smtp.save"}, "address": {"localhost:2525"}, "tls": {"none"}, "username": {"mail-user"}, "password": {"primary-secret-from-ui"}, "bounce_domain": {"bounces.example.test"}}
	for _, check := range []struct {
		Session *http.Cookie
		CSRF    bool
	}{{root, false}, {appAdmin, true}} {
		response := runtimeSettingsRequest(a, check.Session, smtp, check.CSRF)
		if response.Code != http.StatusForbidden {
			t.Fatalf("unauthorized settings save: %d %s", response.Code, response.Body)
		}
	}
	var untouched []byte
	if err := a.DB.QueryRow(ctx, "SELECT runtime_config FROM platform_settings WHERE id=1").Scan(&untouched); err != nil || string(untouched) != "{}" {
		t.Fatalf("rejected request changed configuration: %s %v", untouched, err)
	}
	groups := []url.Values{
		smtp,
		{"action": {"secondary.save"}, "address": {"localhost:2526"}, "tls": {"none"}, "username": {"secondary-user"}, "password": {"secondary-secret-from-ui"}},
		{"action": {"pmg.save"}, "url": {"https://pmg.example.test:8006"}, "username": {"audit@pmg"}, "password": {"gateway-secret-from-ui"}, "nodes": {"node1, node2"}},
		{"action": {"oidc.save"}, "enabled": {"on"}, "issuer": {"https://identity.example.test/.well-known/openid-configuration"}, "provider_name": {"School SSO"}, "client_id": {"edp"}, "client_secret": {"identity-secret-from-ui"}, "public_url": {"http://localhost:8080"}, "scopes": {"openid profile email groups offline_access"}, "groups_claim": {"school_groups"}},
	}
	for _, fields := range groups {
		response := runtimeSettingsRequest(a, root, fields, true)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", fields.Get("action"), response.Code, response.Body)
		}
	}
	c, err := a.runtimeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPPassword != "primary-secret-from-ui" || c.SecondarySMTPPassword != "secondary-secret-from-ui" || c.PMGPassword != "gateway-secret-from-ui" || c.ClientSecret != "identity-secret-from-ui" || c.Issuer != "https://identity.example.test" || c.GroupsClaim != "school_groups" || len(c.PMGNodes) != 2 {
		t.Fatalf("saved configuration did not resolve: %#v", runtimeView(c))
	}
	if a.C.SMTPPassword != "" || a.C.PMGPassword != "" || a.C.ClientSecret != "" {
		t.Fatal("UI settings unexpectedly mutated bootstrap configuration")
	}
	var raw []byte
	if err := a.DB.QueryRow(ctx, "SELECT runtime_config FROM platform_settings WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored RuntimeSettings
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []struct{ Plain, Encrypted string }{
		{c.SMTPPassword, stored.SMTP.PasswordEnc}, {c.SecondarySMTPPassword, stored.Secondary.PasswordEnc}, {c.PMGPassword, stored.PMG.PasswordEnc}, {c.ClientSecret, stored.OIDC.ClientSecretEnc},
	} {
		if strings.Contains(string(raw), secret.Plain) || secret.Encrypted == "" {
			t.Fatal("runtime secret was stored in plaintext or lost")
		}
		plain, err := unseal(a.C.Secret, secret.Encrypted)
		if err != nil || plain != secret.Plain {
			t.Fatal("encrypted secret could not be recovered", err)
		}
	}
	// Blank secret inputs retain the stored value while unrelated groups survive.
	smtp.Set("password", "")
	smtp.Set("address", "localhost:3535")
	if response := runtimeSettingsRequest(a, root, smtp, true); response.Code != http.StatusSeeOther {
		t.Fatalf("blank-secret save: %d %s", response.Code, response.Body)
	}
	c, err = a.runtimeConfig(ctx)
	if err != nil || c.SMTPPassword != "primary-secret-from-ui" || c.ClientSecret != "identity-secret-from-ui" || c.PMGPassword != "gateway-secret-from-ui" || c.SecondarySMTPPassword != "secondary-secret-from-ui" {
		t.Fatal("blank secret or independent-group update lost existing settings", err)
	}
	smtp.Set("password_clear", "on")
	if response := runtimeSettingsRequest(a, root, smtp, true); response.Code != http.StatusSeeOther {
		t.Fatalf("explicit secret clear: %d %s", response.Code, response.Body)
	}
	c, err = a.runtimeConfig(ctx)
	if err != nil || c.SMTPPassword != "" || c.PMGPassword != "gateway-secret-from-ui" || c.ClientSecret != "identity-secret-from-ui" {
		t.Fatal("explicit clear affected other secrets or retained SMTP password", err)
	}
	// Concurrent saves prepared from the same old snapshot must merge by group.
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, group := range []string{"smtp", "pmg"} {
		wg.Add(1)
		go func(group string) {
			defer wg.Done()
			candidate := c
			candidate.SMTPAddress, candidate.PMGUser = "localhost:4545", "changed-audit@pmg"
			errors <- a.saveRuntime(ctx, group, candidate)
		}(group)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	c, err = a.runtimeConfig(ctx)
	if err != nil || c.SMTPAddress != "localhost:4545" || c.PMGUser != "changed-audit@pmg" || c.ClientSecret != "identity-secret-from-ui" {
		t.Fatal("concurrent independent settings updates overwrote each other", err)
	}
}

type runtimeSMTPFixture struct {
	Address string
	Data    atomic.Int64
	Noop    atomic.Int64
	Auth    atomic.Int64
	mu      sync.Mutex
	Bodies  []string
}

func newRuntimeSMTPFixture(t *testing.T, rejectRCPT, username, password string) *runtimeSMTPFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &runtimeSMTPFixture{Address: listener.Addr().String()}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 localhost ESMTP test fixture\r\n")
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					command := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
					switch command {
					case "EHLO":
						fmt.Fprint(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
					case "HELO", "MAIL", "RSET":
						fmt.Fprint(conn, "250 OK\r\n")
					case "AUTH":
						parts := strings.Fields(line)
						var decoded []byte
						if len(parts) == 3 {
							decoded, _ = base64.StdEncoding.DecodeString(parts[2])
						}
						if string(decoded) != "\x00"+username+"\x00"+password {
							fmt.Fprint(conn, "535 Authentication failed\r\n")
						} else {
							fixture.Auth.Add(1)
							fmt.Fprint(conn, "235 Authentication succeeded\r\n")
						}
					case "NOOP":
						fixture.Noop.Add(1)
						fmt.Fprint(conn, "250 OK\r\n")
					case "RCPT":
						if rejectRCPT != "" {
							fmt.Fprintf(conn, "%s\r\n", rejectRCPT)
						} else {
							fmt.Fprint(conn, "250 Recipient accepted\r\n")
						}
					case "DATA":
						fixture.Data.Add(1)
						fmt.Fprint(conn, "354 End with a dot\r\n")
						var body strings.Builder
						for {
							part, err := reader.ReadString('\n')
							if err != nil {
								return
							}
							if part == ".\r\n" {
								break
							}
							body.WriteString(part)
						}
						fixture.mu.Lock()
						fixture.Bodies = append(fixture.Bodies, body.String())
						fixture.mu.Unlock()
						fmt.Fprint(conn, "250 Queued\r\n")
					case "QUIT":
						fmt.Fprint(conn, "221 Bye\r\n")
						return
					default:
						fmt.Fprint(conn, "500 Unsupported test command\r\n")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); wg.Wait() })
	return fixture
}

func TestRuntimeSMTPConnectionAndRealFallback(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	root := runtimeTestSession(t, a, true)
	primary := newRuntimeSMTPFixture(t, "550 5.7.1 Client IP is blocked by Spamhaus", "primary-user", "primary-ui-password")
	secondary := newRuntimeSMTPFixture(t, "", "secondary-user", "secondary-ui-password")
	a.C.SMTPAddress = "127.0.0.1:1"
	a.C.SecondarySMTPAddress = ""
	for _, settings := range []struct{ Group, Address, User, Password string }{
		{"smtp", primary.Address, "primary-user", "primary-ui-password"},
		{"secondary", secondary.Address, "secondary-user", "secondary-ui-password"},
	} {
		fields := url.Values{"action": {settings.Group + ".save"}, "address": {settings.Address}, "tls": {"none"}, "username": {settings.User}, "password": {settings.Password}}
		if settings.Group == "smtp" {
			fields.Set("bounce_domain", "original-bounces.example.test")
		}
		if response := runtimeSettingsRequest(a, root, fields, true); response.Code != http.StatusSeeOther {
			t.Fatalf("save %s: %d %s", settings.Group, response.Code, response.Body)
		}
		fields.Set("action", settings.Group+".test")
		fields.Set("password", "")
		if response := runtimeSettingsRequest(a, root, fields, true); response.Code != http.StatusOK {
			t.Fatalf("test %s: %d %s", settings.Group, response.Code, response.Body)
		}
	}
	if primary.Noop.Load() != 1 || secondary.Noop.Load() != 1 || primary.Auth.Load() != 1 || secondary.Auth.Load() != 1 || primary.Data.Load() != 0 || secondary.Data.Load() != 0 {
		t.Fatal("connection tests did not authenticate and NOOP without submitting DATA")
	}
	if _, err := a.DB.Exec(ctx, "UPDATE applications SET fallback_enabled=true WHERE id='app_demo'"); err != nil {
		t.Fatal(err)
	}
	id, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "runtime@example.test", Template: "welcome", Variables: map[string]string{"name": "Runtime recipient"}}, "runtime-real-fallback")
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.claim(ctx)
	if err != nil || first == nil || first.ID != id || first.Route != "primary" {
		t.Fatal("primary claim failed", err)
	}
	status, detail := a.smtpSend(ctx, *first)
	if status != "failed" || classifyFailure(detail) != "ip_reputation" || primary.Data.Load() != 0 {
		t.Fatalf("primary rejection was not confirmed before DATA: %s %s", status, detail)
	}
	var originalEnvelope string
	if err = a.DB.QueryRow(ctx, "SELECT envelope_sender FROM messages WHERE id=$1", id).Scan(&originalEnvelope); err != nil || originalEnvelope != "bounce+"+id+"@original-bounces.example.test" {
		t.Fatal("worker did not persist the SMTP envelope used by its actual connection", err)
	}
	if err = a.finish(ctx, *first, status, detail); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(ctx, "UPDATE platform_settings SET next_send_at=now() WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(ctx, "UPDATE applications SET next_send_at=now() WHERE id='app_demo'"); err != nil {
		t.Fatal(err)
	}
	next, err := a.claim(ctx)
	if err != nil || next == nil || next.ID != id || next.Route != "secondary" {
		t.Fatal("secondary claim failed", err)
	}
	status, detail = a.smtpSend(ctx, *next)
	if status != "relay_accepted" {
		t.Fatalf("secondary SMTP rejected delivery: %s %s", status, detail)
	}
	if err = a.finish(ctx, *next, status, detail); err != nil {
		t.Fatal(err)
	}
	if err = a.finish(ctx, *first, "failed", "SMTP 550: IP blocked by Spamhaus"); err == nil {
		t.Fatal("stale primary worker could queue another delivery")
	}
	var finalStatus, route string
	var used bool
	var fallbackEvents int
	if err = a.DB.QueryRow(ctx, "SELECT status,route,secondary_used FROM messages WHERE id=$1", id).Scan(&finalStatus, &route, &used); err != nil {
		t.Fatal(err)
	}
	if err = a.DB.QueryRow(ctx, "SELECT count(*) FROM delivery_events WHERE message_id=$1 AND status='fallback_queued'", id).Scan(&fallbackEvents); err != nil {
		t.Fatal(err)
	}
	if finalStatus != "relay_accepted" || route != "secondary" || !used || fallbackEvents != 1 || primary.Data.Load() != 0 || secondary.Data.Load() != 1 {
		t.Fatalf("fallback outcome: status=%s route=%s used=%t events=%d primary DATA=%d secondary DATA=%d", finalStatus, route, used, fallbackEvents, primary.Data.Load(), secondary.Data.Load())
	}
	secondary.mu.Lock()
	body := secondary.Bodies[0]
	secondary.mu.Unlock()
	if !strings.Contains(body, "X-EDP-Message-ID: "+id) || !strings.Contains(body, "To: runtime@example.test") {
		t.Fatal("secondary did not receive the original message identity and recipient")
	}
	if a.C.SMTPAddress != "127.0.0.1:1" || a.C.SecondarySMTPAddress != "" {
		t.Fatal("delivery unexpectedly relied on changed bootstrap settings")
	}
}

func TestRuntimePMGReadOnlyConnectionTest(t *testing.T) {
	a := testApp(t)
	root := runtimeTestSession(t, a, true)
	var logins, reads atomic.Int64
	var historicalID string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api2/json/access/ticket", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.FormValue("username") != "audit@pmg" || r.FormValue("password") != "pmg-password-saved-in-ui" {
			t.Error("PMG test did not authenticate with saved runtime credentials")
			problem(w, 401, "invalid fixture credentials")
			return
		}
		logins.Add(1)
		respond(w, 200, map[string]any{"data": map[string]string{"ticket": "runtime-pmg-ticket"}})
	})
	mux.HandleFunc("GET /api2/json/nodes/fixture/tracker", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("PMGAuthCookie")
		if err != nil || cookie.Value != "runtime-pmg-ticket" || r.URL.Query().Get("starttime") == "" || r.URL.Query().Get("endtime") == "" {
			t.Error("PMG read-only test omitted authentication or bounded tracker query")
			problem(w, 400, "invalid fixture tracker request")
			return
		}
		reads.Add(1)
		if r.URL.Query().Get("limit") == "2000" && historicalID != "" {
			envelope := "bounce+" + historicalID + "@old-bounces.example.test"
			if r.URL.Query().Get("from") != envelope || r.URL.Query().Get("target") != "historical@example.test" {
				t.Error("tracking used current configuration instead of the persisted SMTP envelope")
				problem(w, 400, "historical envelope mismatch")
				return
			}
			respond(w, 200, map[string]any{"data": []pmgRecord{{ID: "historical-queue", MsgID: "<" + historicalID + "@localhost>", From: envelope, To: "historical@example.test", DStatus: "A", RStatus: "2", Time: time.Now().Unix()}}})
			return
		}
		if r.URL.Query().Get("limit") != "1" {
			t.Error("connection test did not bound the tracker query")
			problem(w, 400, "query limit mismatch")
			return
		}
		respond(w, 200, map[string]any{"data": []pmgRecord{}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected PMG operation %s %s", r.Method, r.URL.Path)
		problem(w, 405, "fixture only allows ticket login and tracker reads")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	fields := url.Values{"action": {"pmg.save"}, "url": {server.URL}, "username": {"audit@pmg"}, "password": {"pmg-password-saved-in-ui"}, "nodes": {"fixture"}}
	if response := runtimeSettingsRequest(a, root, fields, true); response.Code != http.StatusSeeOther {
		t.Fatalf("PMG save: %d %s", response.Code, response.Body)
	}
	fields.Set("action", "pmg.test")
	fields.Set("password", "")
	response := runtimeSettingsRequest(a, root, fields, true)
	if response.Code != http.StatusOK || logins.Load() != 1 || reads.Load() != 1 {
		t.Fatalf("PMG test: %d %s logins=%d reads=%d", response.Code, response.Body, logins.Load(), reads.Load())
	}
	if strings.Contains(response.Body.String(), "pmg-password-saved-in-ui") || a.C.PMGURL != "" || a.C.PMGPassword != "" {
		t.Fatal("PMG test exposed secret or mutated bootstrap environment")
	}
	var count int
	if err := a.DB.QueryRow(context.Background(), "SELECT count(*) FROM messages").Scan(&count); err != nil || count != 0 {
		t.Fatal("PMG connection test changed the message queue", err)
	}
	ctx := context.Background()
	var err error
	historicalID, _, err = a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "historical@example.test", Template: "welcome", Variables: map[string]string{"name": "Historical recipient"}}, "historical-runtime-envelope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(ctx, "UPDATE messages SET status='relay_accepted',envelope_sender=$2,relay_accepted_at=now() WHERE id=$1", historicalID, "bounce+"+historicalID+"@old-bounces.example.test"); err != nil {
		t.Fatal(err)
	}
	changed := url.Values{"action": {"smtp.save"}, "address": {"localhost:2525"}, "tls": {"none"}, "bounce_domain": {"new-bounces.example.test"}}
	if response := runtimeSettingsRequest(a, root, changed, true); response.Code != http.StatusSeeOther {
		t.Fatalf("bounce domain update: %d %s", response.Code, response.Body)
	}
	live, err := a.runtimeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	poller := *a
	poller.C = live
	if err = poller.PollPMG(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = a.DB.QueryRow(ctx, "SELECT status FROM messages WHERE id=$1", historicalID).Scan(&status); err != nil || status != "remote_accepted" {
		t.Fatal("UI-configured PMG could not correlate a message after bounce-domain changes", err)
	}
}

func TestRuntimeSettingsMultipartConnectionTest(t *testing.T) {
	a := testApp(t)
	root := runtimeTestSession(t, a, true)
	smtp := newRuntimeSMTPFixture(t, "", "", "")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for field, value := range map[string]string{"action": "smtp.test", "address": smtp.Address, "tls": "none"} {
		if err := writer.WriteField(field, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-CSRF-Token", "runtime-csrf")
	req.AddCookie(root)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || smtp.Noop.Load() != 1 || smtp.Data.Load() != 0 {
		t.Fatalf("multipart connection test: %d %s NOOP=%d DATA=%d", response.Code, response.Body, smtp.Noop.Load(), smtp.Data.Load())
	}
}

func TestRuntimeDisabledSSORetainsCredentials(t *testing.T) {
	a := testApp(t)
	root := runtimeTestSession(t, a, true)
	form := url.Values{"action": {"oidc.save"}, "issuer": {"https://identity.example.test"}, "client_id": {"edp"}, "client_secret": {"stored-secret"}, "public_url": {"http://localhost:8080"}, "scopes": {"openid profile"}}
	if r := runtimeSettingsRequest(a, root, form, true); r.Code != 303 {
		t.Fatalf("save disabled SSO: %d %s", r.Code, r.Body)
	}
	c, e := a.runtimeConfig(context.Background())
	if e != nil || c.ssoEnabled() || c.ClientID != "edp" || c.ClientSecret != "stored-secret" {
		t.Fatal("disabled settings lost credentials or enabled login", e)
	}
	page := httptest.NewRecorder()
	a.Handler().ServeHTTP(page, httptest.NewRequest("GET", "/login", nil))
	if strings.Contains(page.Body.String(), `href="/auth/sso"`) {
		t.Fatal("disabled SSO offered login")
	}
	form.Set("enabled", "on")
	form.Set("client_secret", "")
	if r := runtimeSettingsRequest(a, root, form, true); r.Code != 303 {
		t.Fatalf("re-enable SSO: %d %s", r.Code, r.Body)
	}
	c, e = a.runtimeConfig(context.Background())
	if e != nil || !c.ssoEnabled() || c.ClientSecret != "stored-secret" {
		t.Fatal("re-enable lost credentials", e)
	}
}
