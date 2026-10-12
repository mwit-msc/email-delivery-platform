package edp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Webhook receivers must deduplicate EDP-Event-ID within an endpoint. Delivery
// is at least once: a receiver may accept a request just before its lease expires.
const webhookAttempts = 8

var webhookBlockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001:20::/28"),
}

func webhookPublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Only the allocated global IPv6 unicast range is eligible.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range webhookBlockedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func parseWebhookURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("webhook URL must be an HTTPS URL without credentials or fragments")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid webhook HTTPS port")
		}
	}
	return u, nil
}

func webhookAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("webhook hostname could not be resolved")
	}
	for _, ip := range addresses {
		if !webhookPublicAddress(ip) {
			return nil, errors.New("webhook hostname must resolve exclusively to public IP addresses")
		}
	}
	return addresses, nil
}

func validateWebhookURL(ctx context.Context, raw string) error {
	u, err := parseWebhookURL(raw)
	if err != nil {
		return err
	}
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = webhookAddresses(lookup, u.Hostname())
	return err
}

func webhookClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid webhook address")
			}
			ips, err := webhookAddresses(ctx, host)
			if err != nil {
				return nil, err
			}
			dialer := net.Dialer{Timeout: 5 * time.Second}
			for _, ip := range ips {
				// Dial the validated address rather than resolving the hostname again.
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
			}
			return nil, errors.New("webhook connection failed")
		},
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (a *App) saveWebhook(ctx context.Context, app, id, name, rawURL, secret string, enabled bool) (string, error) {
	name, rawURL = strings.TrimSpace(name), strings.TrimSpace(rawURL)
	if name == "" || len(name) > 100 {
		return "", errors.New("webhook name must contain 1–100 bytes")
	}
	if err := validateWebhookURL(ctx, rawURL); err != nil {
		return "", err
	}
	if (secret != "" && len(secret) < 32) || len(secret) > 1024 || (id == "" && secret == "") {
		return "", errors.New("new signing secrets must contain 32–1024 bytes; leave blank to retain an existing secret")
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var scope string
	if err = tx.QueryRow(ctx, "SELECT id FROM applications WHERE id=$1 FOR UPDATE", app).Scan(&scope); err != nil {
		return "", errors.New("application not found")
	}
	if id == "" {
		var count int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM webhook_endpoints WHERE application_id=$1", app).Scan(&count); err != nil {
			return "", err
		}
		if count >= 20 {
			return "", errors.New("an application can configure at most 20 webhooks")
		}
		id = newID("wh_")
		_, err = tx.Exec(ctx, "INSERT INTO webhook_endpoints(id,application_id,name,url,secret,enabled) VALUES($1,$2,$3,$4,$5,$6)", id, app, name, rawURL, seal(a.C.Secret, secret), enabled)
	} else {
		var encrypted string
		if secret != "" {
			encrypted = seal(a.C.Secret, secret)
		}
		result, updateErr := tx.Exec(ctx, "UPDATE webhook_endpoints SET name=$3,url=$4,secret=CASE WHEN $5='' THEN secret ELSE $5 END,enabled=$6 WHERE id=$1 AND application_id=$2", id, app, name, rawURL, encrypted, enabled)
		err = updateErr
		if err == nil && result.RowsAffected() != 1 {
			err = errors.New("webhook not found in this application")
		}
	}
	if err != nil {
		return "", err
	}
	if !enabled {
		_, err = tx.Exec(ctx, "UPDATE webhook_jobs SET status='cancelled',lease_until=NULL,lease_token='' WHERE endpoint_id=$1 AND status IN ('queued','sending')", id)
		if err != nil {
			return "", err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (a *App) listWebhooks(ctx context.Context, app string) ([]map[string]any, error) {
	rows, err := a.DB.Query(ctx, `SELECT e.id,e.application_id,e.name,e.url,e.enabled,e.secret<>'',e.created_at,
		(SELECT count(*) FROM webhook_jobs j WHERE j.endpoint_id=e.id AND j.status IN ('queued','sending')),
		(SELECT count(*) FROM webhook_jobs j WHERE j.endpoint_id=e.id AND j.status='failed')
		FROM webhook_endpoints e WHERE application_id=$1 ORDER BY created_at,id`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, application, name, rawURL string
		var enabled, secretSet bool
		var created time.Time
		var queued, failed int64
		if err = rows.Scan(&id, &application, &name, &rawURL, &enabled, &secretSet, &created, &queued, &failed); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"id": id, "application_id": application, "name": name, "url": rawURL, "enabled": enabled, "secret_set": secretSet, "created_at": created, "queued": queued, "failed": failed})
	}
	return result, rows.Err()
}

type webhookJob struct {
	ID, EventID                  int64
	Endpoint, Token, URL, Secret string
	Payload                      []byte
	Attempts                     int
}

func (a *App) claimWebhook(ctx context.Context) (*webhookJob, error) {
	token := newID("lease_")
	job := &webhookJob{Token: token}
	err := a.DB.QueryRow(ctx, `WITH candidate AS (
		SELECT j.id FROM webhook_jobs j JOIN webhook_endpoints e ON e.id=j.endpoint_id
		WHERE e.enabled AND j.attempts<$2 AND j.available_at<=now()
		AND (j.status='queued' OR (j.status='sending' AND j.lease_until<now()))
		ORDER BY j.available_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1
	), claimed AS (
		UPDATE webhook_jobs j SET status='sending',attempts=attempts+1,lease_until=now()+interval '1 minute',lease_token=$1
		FROM candidate c WHERE j.id=c.id RETURNING j.*
	)
	SELECT j.id,j.event_id,j.endpoint_id,j.payload,j.attempts,e.url,e.secret
	FROM claimed j JOIN webhook_endpoints e ON e.id=j.endpoint_id`, token, webhookAttempts).
		Scan(&job.ID, &job.EventID, &job.Endpoint, &job.Payload, &job.Attempts, &job.URL, &job.Secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

func sendWebhook(ctx context.Context, client *http.Client, job *webhookJob, secret string, now time.Time) error {
	if _, err := parseWebhookURL(job.URL); err != nil {
		return errors.New("webhook URL is invalid")
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(job.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.URL, bytes.NewReader(job.Payload))
	if err != nil {
		return errors.New("webhook request could not be created")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "EDP-Webhook/1")
	req.Header.Set("EDP-Event-ID", strconv.FormatInt(job.EventID, 10))
	req.Header.Set("EDP-Timestamp", timestamp)
	req.Header.Set("EDP-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	response, err := client.Do(req)
	if err != nil {
		// http.Client errors include the request URL, which may contain a token.
		return errors.New("webhook request failed or timed out")
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("receiver returned HTTP %d", response.StatusCode)
	}
	return nil
}

func webhookBackoff(attempt int) time.Duration {
	delay := 30 * time.Minute
	for n := 1; n < attempt && delay < 6*time.Hour; n++ {
		delay *= 2
	}
	if delay > 6*time.Hour {
		delay = 6 * time.Hour
	}
	return delay
}

func (a *App) finishWebhook(ctx context.Context, job *webhookJob, deliveryErr error) error {
	status, detail, delay := "delivered", "", time.Duration(0)
	if deliveryErr != nil {
		status, detail, delay = "queued", deliveryErr.Error(), webhookBackoff(job.Attempts)
		if job.Attempts >= webhookAttempts {
			status = "failed"
		}
	}
	_, err := a.DB.Exec(ctx, `UPDATE webhook_jobs SET status=$3,last_error=$4,available_at=now()+$5*interval '1 second',lease_until=NULL,lease_token=''
		WHERE id=$1 AND lease_token=$2 AND status='sending'`, job.ID, job.Token, status, detail, delay.Seconds())
	return err
}

func (a *App) webhookLoop(ctx context.Context) {
	client := webhookClient()
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		// Exhausted expired leases need a terminal outcome even after a crash.
		a.DB.Exec(ctx, "UPDATE webhook_jobs SET status='failed',last_error='Delivery lease expired after the final attempt',lease_until=NULL,lease_token='' WHERE status='sending' AND attempts>=$1 AND lease_until<now()", webhookAttempts)
		job, err := a.claimWebhook(ctx)
		if err == nil && job != nil {
			var enabled bool
			// A settings change may disable an endpoint after the queue claim.
			if err = a.DB.QueryRow(ctx, "SELECT enabled FROM webhook_endpoints WHERE id=$1", job.Endpoint).Scan(&enabled); err == nil && enabled {
				secret, decryptErr := unseal(a.C.Secret, job.Secret)
				if decryptErr != nil || secret == "" {
					err = errors.New("webhook signing secret is unavailable")
				} else {
					err = sendWebhook(ctx, client, job, secret, time.Now())
				}
				a.finishWebhook(ctx, job, err)
			} else if err == nil {
				a.DB.Exec(ctx, "UPDATE webhook_jobs SET status='cancelled',lease_until=NULL,lease_token='' WHERE id=$1 AND lease_token=$2", job.ID, job.Token)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
