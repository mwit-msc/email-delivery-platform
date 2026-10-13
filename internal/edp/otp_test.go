package edp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http/httptest"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func otpTestCode(t *testing.T, a *App, id string) string {
	t.Helper()
	var sealed string
	if err := a.DB.QueryRow(context.Background(), "SELECT plain FROM messages WHERE id=$1", id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	body, err := unseal(a.C.Secret, sealed)
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`\b[0-9]{6}\b`).FindString(body)
	if code == "" {
		t.Fatal("test OTP was not present in encrypted body")
	}
	return code
}

func otpAPIRequest(t *testing.T, a *App, token, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestOTPAPIPrivacyAndSingleUse(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	creator := automationKey(t, a, "app_demo", []string{"otp.create"}, root)
	verifier := automationKey(t, a, "app_demo", []string{"otp.verify"}, root)
	in := OTPInput{To: "person@example.test", Purpose: "login"}
	if code := otpAPIRequest(t, a, creator, "/v1/otp/challenges", "", in).Code; code != 400 {
		t.Fatal("missing idempotency accepted", code)
	}
	if code := otpAPIRequest(t, a, verifier, "/v1/otp/challenges", "create", in).Code; code != 403 {
		t.Fatal("verifier created OTP", code)
	}
	first := otpAPIRequest(t, a, creator, "/v1/otp/challenges", "create", in)
	obj := automationObject(t, first, 202)
	id, mid := obj["id"].(string), obj["message_id"].(string)
	code := otpTestCode(t, a, mid)
	if _, found := obj["code"]; found || first.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("creation exposed code or cached response")
	}
	var subject, html, plain string
	var encrypted bool
	var digest []byte
	a.DB.QueryRow(ctx, "SELECT subject,html,plain,otp_encrypted FROM messages WHERE id=$1", mid).Scan(&subject, &html, &plain, &encrypted)
	a.DB.QueryRow(ctx, "SELECT code_hash FROM otp_challenges WHERE id=$1", id).Scan(&digest)
	if !encrypted || len(digest) != 32 || strings.Contains(subject, code) || strings.Contains(html, "<p>") || strings.Contains(plain, "verification code") {
		t.Fatal("generated OTP was not protected at rest")
	}
	session := runtimeTestSession(t, a, true)
	r := httptest.NewRequest("GET", "/messages/"+mid+"/preview", nil)
	r.AddCookie(session)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Verification code content is hidden") || strings.Contains(w.Body.String(), code) || strings.Contains(w.Body.String(), html) {
		t.Fatal("preview exposed code or ciphertext", w.Code)
	}
	replay := automationObject(t, otpAPIRequest(t, a, creator, "/v1/otp/challenges", "create", in), 202)
	if replay["id"] != id || replay["expires_at"] != obj["expires_at"] || replay["replayed"] != true {
		t.Fatal("idempotent replay regenerated challenge", replay)
	}
	if response := otpAPIRequest(t, a, creator, "/v1/otp/challenges", "second", in); response.Code != 429 || response.Header().Get("Retry-After") == "" {
		t.Fatal("resend cooldown bypassed", response.Code)
	}
	if response := otpAPIRequest(t, a, creator, "/v1/otp/challenges", "create", OTPInput{To: in.To, Purpose: "reset"}); response.Code != 409 {
		t.Fatal("changed idempotency payload accepted", response.Code)
	}
	path := "/v1/otp/challenges/" + id + "/verify"
	if response := otpAPIRequest(t, a, creator, path, "", map[string]string{"purpose": "login", "code": code}); response.Code != 403 {
		t.Fatal("creator used verification scope", response.Code)
	}
	wrongPurpose := automationObject(t, otpAPIRequest(t, a, verifier, path, "", map[string]string{"purpose": "reset", "code": code}), 200)
	if wrongPurpose["verified"] != false {
		t.Fatal("OTP crossed purpose boundary")
	}
	if _, err := a.DB.Exec(ctx, "INSERT INTO applications(id,name,from_address) VALUES('app_other','Other','sender@example.test')"); err != nil {
		t.Fatal(err)
	}
	other := automationKey(t, a, "app_other", []string{"otp.verify"}, root)
	foreign := automationObject(t, otpAPIRequest(t, a, other, path, "", map[string]string{"purpose": "login", "code": code}), 200)
	if foreign["verified"] != false {
		t.Fatal("OTP crossed application boundary")
	}
	// Concurrent requests must not consume the same challenge twice.
	var successes atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := a.verifyOTP(ctx, "app_demo", id, "login", code)
			if err != nil {
				t.Error(err)
			}
			if result.Verified {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("challenge consumed more than once", successes.Load())
	}
	var remaining string
	a.DB.QueryRow(ctx, "SELECT plain FROM messages WHERE id=$1", mid).Scan(&remaining)
	if remaining != "" {
		t.Fatal("consumed OTP retained queue body")
	}
	replay = automationObject(t, otpAPIRequest(t, a, creator, "/v1/otp/challenges", "create", in), 202)
	if replay["id"] != id {
		t.Fatal("replay reset consumed challenge")
	}
	result, err := a.verifyOTP(ctx, "app_demo", id, "login", code)
	if err != nil || result.Verified {
		t.Fatal("consumed OTP verified again", err)
	}
}

func TestOTPAttemptsExpiryAndMaintenance(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	c, err := a.createOTP(ctx, "app_demo", OTPInput{"locked@example.test", "login"}, "attempts")
	if err != nil {
		t.Fatal(err)
	}
	code := otpTestCode(t, a, c.MessageID)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if result, err := a.verifyOTP(ctx, "app_demo", c.ID, "login", "incorrect"); err != nil || result.Verified {
				t.Error("incorrect OTP accepted", err)
			}
		}()
	}
	wg.Wait()
	var attempts int
	a.DB.QueryRow(ctx, "SELECT attempts FROM otp_challenges WHERE id=$1", c.ID).Scan(&attempts)
	if attempts != 5 {
		t.Fatal("concurrent attempts bypassed limit", attempts)
	}
	if result, err := a.verifyOTP(ctx, "app_demo", c.ID, "login", code); err != nil || result.Verified {
		t.Fatal("locked code verified", err)
	}
	c, err = a.createOTP(ctx, "app_demo", OTPInput{"expired@example.test", "login"}, "expired")
	if err != nil {
		t.Fatal(err)
	}
	code = otpTestCode(t, a, c.MessageID)
	a.DB.Exec(ctx, "UPDATE otp_challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", c.ID)
	if result, err := a.verifyOTP(ctx, "app_demo", c.ID, "login", code); err != nil || result.Verified {
		t.Fatal("expired code verified", err)
	}
	a.maintenance(ctx)
	var digest []byte
	var plain string
	a.DB.QueryRow(ctx, "SELECT code_hash FROM otp_challenges WHERE id=$1", c.ID).Scan(&digest)
	a.DB.QueryRow(ctx, "SELECT plain FROM messages WHERE id=$1", c.MessageID).Scan(&plain)
	if len(digest) != 0 || plain != "" {
		t.Fatal("expired secrets survived maintenance")
	}
}

func TestOTPResendRollbackAndClaimRace(t *testing.T) {
	a := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in := OTPInput{"resend@example.test", "login"}
	old, err := a.createOTP(ctx, "app_demo", in, "first")
	if err != nil {
		t.Fatal(err)
	}
	code := otpTestCode(t, a, old.MessageID)
	a.DB.Exec(ctx, "UPDATE otp_challenges SET created_at=clock_timestamp()-interval '61 seconds' WHERE id=$1", old.ID)
	a.DB.Exec(ctx, "UPDATE applications SET daily_limit=1 WHERE id='app_demo'")
	if _, err := a.createOTP(ctx, "app_demo", in, "failed-replacement"); err == nil {
		t.Fatal("OTP bypassed daily quota")
	}
	var revoked *time.Time
	var digest []byte
	a.DB.QueryRow(ctx, "SELECT revoked_at,code_hash FROM otp_challenges WHERE id=$1", old.ID).Scan(&revoked, &digest)
	if revoked != nil || len(digest) != 32 || otpTestCode(t, a, old.MessageID) != code {
		t.Fatal("failed admission revoked prior challenge")
	}
	a.DB.Exec(ctx, "UPDATE applications SET daily_limit=100 WHERE id='app_demo'")
	// Simulate a worker holding the old message before taking the app lock.
	claimTx, err := a.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer claimTx.Rollback(ctx)
	var mid string
	if err = claimTx.QueryRow(ctx, "SELECT id FROM messages WHERE id=$1 FOR UPDATE", old.MessageID).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	newChallenge, err := a.createOTP(ctx, "app_demo", in, "replacement")
	if err != nil {
		t.Fatal("resend waited for worker-owned message", err)
	}
	if _, err = claimTx.Exec(ctx, "UPDATE messages SET status='sending',lease_token='otp-race' WHERE id=$1", old.MessageID); err != nil {
		t.Fatal(err)
	}
	if err = claimTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if result, err := a.verifyOTP(ctx, "app_demo", old.ID, "login", code); err != nil || result.Verified {
		t.Fatal("superseded code verified", err)
	}
	status, _ := a.smtpSend(ctx, delivery{ID: old.MessageID, OTPEncrypted: true, Recipient: in.To, From: "notifications@example.test"})
	if status != "expired" {
		t.Fatal("worker submitted retired OTP", status)
	}
	if newChallenge.ID == old.ID {
		t.Fatal("resend reused old challenge")
	}
}

func TestOTPConcurrentCreationAndSMTPDelivery(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	fixture := newRuntimeSMTPFixture(t, "", "", "")
	a.C.SMTPAddress, a.C.SMTPTLS = fixture.Address, "none"
	var wg sync.WaitGroup
	results := make(chan OTPChallenge, 10)
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := a.createOTP(ctx, "app_demo", OTPInput{"concurrent@example.test", []string{"login", "reset"}[i%2]}, "same-recipient-"+string(rune('a'+i)))
			if err == nil {
				results <- result
			} else if httpErr, ok := err.(*HTTPError); !ok || httpErr.Code != 429 {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(results)
	if len(results) != 1 {
		t.Fatal("concurrent cross-purpose requests bypassed cooldown", len(results))
	}
	c := <-results
	code := otpTestCode(t, a, c.MessageID)
	d, err := a.claim(ctx)
	if err != nil || d == nil || !d.OTPEncrypted || d.ID != c.MessageID {
		t.Fatal("encrypted OTP not claimed", err)
	}
	status, detail := a.smtpSend(ctx, *d)
	if status != "relay_accepted" || fixture.Data.Load() != 1 {
		t.Fatal("SMTP did not accept decrypted OTP", status, detail)
	}
	fixture.mu.Lock()
	body := fixture.Bodies[0]
	fixture.mu.Unlock()
	message, err := mail.ReadMessage(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	part, err := multipart.NewReader(message.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), code) || strings.Contains(body, "Bcc:") || len(d.CC)+len(d.BCC) != 0 {
		t.Fatal("OTP content or single-recipient envelope incorrect")
	}
	if err = a.finish(ctx, *d, status, detail); err != nil {
		t.Fatal(err)
	}
}

func TestOTPVerifierBindsChallengeContext(t *testing.T) {
	original := otpDigest(make([]byte, 32), "otp_test", "app_demo", "person@example.test", "login", "000012")
	for _, parts := range [][]string{
		{"otp_other", "app_demo", "person@example.test", "login", "000012"},
		{"otp_test", "app_other", "person@example.test", "login", "000012"},
		{"otp_test", "app_demo", "other@example.test", "login", "000012"},
		{"otp_test", "app_demo", "person@example.test", "reset", "000012"},
		{"otp_test", "app_demo", "person@example.test", "login", "12"},
	} {
		if bytes.Equal(original, otpDigest(make([]byte, 32), parts[0], parts[1], parts[2], parts[3], parts[4])) {
			t.Fatal("OTP context was not bound")
		}
	}
}

func TestOTPExpiryAfterVerificationLockWait(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	c, err := a.createOTP(ctx, "app_demo", OTPInput{"wait@example.test", "login"}, "wait")
	if err != nil {
		t.Fatal(err)
	}
	code := otpTestCode(t, a, c.MessageID)
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "UPDATE otp_challenges SET expires_at=clock_timestamp()+interval '300 milliseconds' WHERE id=$1", c.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan OTPVerification, 1)
	go func() {
		result, err := a.verifyOTP(ctx, "app_demo", c.ID, "login", code)
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	time.Sleep(500 * time.Millisecond)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if (<-done).Verified {
		t.Fatal("verification used time before its row-lock wait")
	}
}
