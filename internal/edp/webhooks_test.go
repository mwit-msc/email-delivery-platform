package edp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type webhookRoundTripper func(*http.Request) (*http.Response, error)

func (f webhookRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebhookDestinationRestrictions(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.169.254", "100.100.100.200", "100.64.1.2", "0.1.2.3", "224.0.0.1", "240.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "::", "::1", "fc00::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "2001:db8::1", "2002:7f00:1::", "64:ff9b::7f00:1", "2001::1"} {
		if webhookPublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("unsafe address accepted: %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !webhookPublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("public address rejected: %s", raw)
		}
	}
	for _, raw := range []string{"http://example.test/events", "https://user:password@example.test", "https://example.test/#fragment", "https://example.test:99999", "https://127.0.0.1/events", "https://[::1]/events", "https://169.254.169.254/metadata"} {
		if err := validateWebhookURL(context.Background(), raw); err == nil {
			t.Errorf("unsafe URL accepted: %s", raw)
		}
	}
	// This is only address validation, never an outgoing HTTP request.
	if err := validateWebhookURL(context.Background(), "https://8.8.8.8/events"); err != nil {
		t.Fatal(err)
	}
	client := webhookClient()
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatal("redirects enabled")
	}
	transport := client.Transport.(*http.Transport)
	if _, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:443"); err == nil {
		t.Fatal("dial-time validation allowed a private destination")
	}
}

func TestWebhookSignedDeliveryAndSafeErrors(t *testing.T) {
	job := &webhookJob{EventID: 42, URL: "https://receiver.example.test/events?token=PRIVATE", Payload: []byte(`{"event_id":42,"status":"remote_accepted"}`)}
	secret := strings.Repeat("secret", 6)
	client := &http.Client{Transport: webhookRoundTripper(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || string(body) != string(job.Payload) || request.Header.Get("EDP-Event-ID") != "42" || request.Header.Get("Content-Type") != "application/json" {
			t.Fatal("payload or event identity changed")
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("1700000000."))
		mac.Write(body)
		if request.Header.Get("EDP-Timestamp") != "1700000000" || request.Header.Get("EDP-Signature") != "v1="+hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("signature does not cover the timestamp and exact body")
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err := sendWebhook(context.Background(), client, job, secret, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	client.Transport = webhookRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("https://receiver.example.test/events?token=PRIVATE exploded")
	})
	if err := sendWebhook(context.Background(), client, job, secret, time.Now()); err == nil || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "receiver") {
		t.Fatal("request URL leaked in delivery error")
	}
	client.Transport = webhookRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("secret server response"))}, nil
	})
	if err := sendWebhook(context.Background(), client, job, secret, time.Now()); err == nil || err.Error() != "receiver returned HTTP 500" {
		t.Fatal("status failure not retained safely", err)
	}
	var retryWindow time.Duration
	for attempt := 1; attempt < webhookAttempts; attempt++ {
		retryWindow += webhookBackoff(attempt)
	}
	if retryWindow < 24*time.Hour || webhookBackoff(100) > 6*time.Hour {
		t.Fatal("retry window is shorter than a day or backoff is unbounded", retryWindow)
	}
}

func TestWebhookPersistenceAndDeliveryEvents(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	secret := strings.Repeat("private-signing-secret", 2)
	id, err := a.saveWebhook(ctx, "app_demo", "", "Delivery events", "https://8.8.8.8/events", secret, true)
	if err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := a.DB.QueryRow(ctx, "SELECT secret FROM webhook_endpoints WHERE id=$1", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if encrypted == secret {
		t.Fatal("signing secret stored in plaintext")
	}
	if _, err := a.saveWebhook(ctx, "app_demo", id, "Updated endpoint", "https://8.8.8.8/events", "", true); err != nil {
		t.Fatal(err)
	}
	var kept string
	a.DB.QueryRow(ctx, "SELECT secret FROM webhook_endpoints WHERE id=$1", id).Scan(&kept)
	if kept != encrypted {
		t.Fatal("blank form input replaced the existing secret")
	}
	if _, err := a.saveWebhook(ctx, "app_demo", "", "Weak secret", "https://8.8.8.8/events", "too short", true); err == nil {
		t.Fatal("weak signing secret accepted")
	}
	a.DB.Exec(ctx, "INSERT INTO applications(id,name,from_address) VALUES('app_other','Other','sender@example.test')")
	if _, err := a.saveWebhook(ctx, "app_other", id, "Stolen endpoint", "https://8.8.8.8/events", "", true); err == nil {
		t.Fatal("cross-application edit allowed")
	}
	message, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "synthetic@example.test", Template: "welcome", Variables: map[string]string{"name": "Synthetic"}}, "webhook-test")
	if err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := a.DB.QueryRow(ctx, "INSERT INTO delivery_events(message_id,status,detail) VALUES($1,'remote_accepted','Accepted by remote server') RETURNING id", message).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	var count int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM webhook_jobs WHERE endpoint_id=$1 AND event_id=$2", id, eventID).Scan(&count)
	if count != 1 {
		t.Fatal("delivery event did not enqueue exactly one job", count)
	}
	// Admission can also record a queued event. Isolate the event under test.
	a.DB.Exec(ctx, "UPDATE webhook_jobs SET status='delivered' WHERE event_id<>$1", eventID)
	job, err := a.claimWebhook(ctx)
	if err != nil || job == nil || job.EventID != eventID || job.Attempts != 1 || len(job.Payload) == 0 {
		t.Fatal("durable event claim failed", job, err)
	}
	other, err := a.claimWebhook(ctx)
	if err != nil || other != nil {
		t.Fatal("active lease was claimed twice", other, err)
	}
	if err := a.finishWebhook(ctx, job, errors.New("receiver returned HTTP 503")); err != nil {
		t.Fatal(err)
	}
	var status string
	var available time.Time
	a.DB.QueryRow(ctx, "SELECT status,available_at FROM webhook_jobs WHERE id=$1", job.ID).Scan(&status, &available)
	if status != "queued" || time.Until(available) < 29*time.Minute {
		t.Fatal("delivery failure did not schedule a durable retry", status, available)
	}
	a.DB.Exec(ctx, "UPDATE webhook_jobs SET available_at=now() WHERE id=$1", job.ID)
	second, err := a.claimWebhook(ctx)
	if err != nil || second == nil || second.Attempts != 2 || second.EventID != eventID || string(second.Payload) != string(job.Payload) {
		t.Fatal("retry changed the stable event or body", second, err)
	}
	// A previous owner cannot acknowledge a successor's lease.
	a.finishWebhook(ctx, job, nil)
	a.DB.QueryRow(ctx, "SELECT status FROM webhook_jobs WHERE id=$1", second.ID).Scan(&status)
	if status != "sending" {
		t.Fatal("stale lease owner acknowledged a newer attempt")
	}
	a.finishWebhook(ctx, second, nil)
	a.DB.QueryRow(ctx, "SELECT status FROM webhook_jobs WHERE id=$1", second.ID).Scan(&status)
	if status != "delivered" {
		t.Fatal("successful webhook was not acknowledged")
	}
	list, err := a.listWebhooks(ctx, "app_demo")
	if err != nil || len(list) != 1 || list[0]["secret_set"] != true {
		t.Fatal("webhook listing failed", list, err)
	}
	if _, ok := list[0]["secret"]; ok {
		t.Fatal("listing exposed an encrypted secret")
	}
	a.DB.Exec(ctx, "UPDATE webhook_jobs SET status='queued',attempts=7,available_at=now() WHERE id=$1", job.ID)
	last, err := a.claimWebhook(ctx)
	if err != nil || last == nil || last.Attempts != webhookAttempts {
		t.Fatal("final attempt was not claimable", last, err)
	}
	a.finishWebhook(ctx, last, errors.New("receiver returned HTTP 503"))
	a.DB.QueryRow(ctx, "SELECT status FROM webhook_jobs WHERE id=$1", job.ID).Scan(&status)
	if status != "failed" {
		t.Fatal("retry exhaustion was not terminal")
	}
	a.DB.Exec(ctx, "UPDATE webhook_jobs SET status='queued',attempts=0 WHERE id=$1", job.ID)
	if _, err := a.saveWebhook(ctx, "app_demo", id, "Disabled endpoint", "https://8.8.8.8/events", "", false); err != nil {
		t.Fatal(err)
	}
	a.DB.QueryRow(ctx, "SELECT status FROM webhook_jobs WHERE id=$1", job.ID).Scan(&status)
	if status != "cancelled" {
		t.Fatal("disabling an endpoint left its queue active")
	}
	if _, err := a.DB.Exec(ctx, "INSERT INTO delivery_events(message_id,status) VALUES($1,'bounced')", message); err != nil {
		t.Fatal(err)
	}
	a.DB.QueryRow(ctx, "SELECT count(*) FROM webhook_jobs WHERE endpoint_id=$1 AND event_id>$2", id, eventID).Scan(&count)
	if count != 0 {
		t.Fatal("disabled endpoint received an additional event", count)
	}
}
