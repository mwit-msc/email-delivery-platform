package edp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

type delivery struct {
	ID, AppID, Recipient, From, FromName, Reply, Subject, HTML, Plain, Lease, Route, RelayID, FallbackRelayID string
	Attempts                                                                                                  int
	CC                                                                                                        []string
}

func (a *App) claim(ctx context.Context) (*delivery, error) {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	var paused bool
	var rate, connections int
	var dispatchCounter int64
	var next time.Time
	var circuit *time.Time
	e = tx.QueryRow(ctx, "SELECT paused,rate_per_minute,max_connections,next_send_at,circuit_until,dispatch_counter FROM platform_settings WHERE id=1 FOR UPDATE").Scan(&paused, &rate, &connections, &next, &circuit, &dispatchCounter)
	if e != nil {
		return nil, e
	}
	if paused || next.After(time.Now()) || circuit != nil && circuit.After(time.Now()) {
		return nil, nil
	}
	var active int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM messages WHERE status='sending'").Scan(&active); e != nil {
		return nil, e
	}
	if active >= connections {
		return nil, nil
	}
	d := &delivery{}
	e = tx.QueryRow(ctx, `SELECT m.id,m.application_id,m.recipient,m.from_address,m.from_name,m.reply_to,m.subject,m.html,m.plain,m.route_attempts,m.route,coalesce(m.relay_id,''),coalesce(m.fallback_relay_id,'') ,m.cc FROM messages m JOIN applications a ON a.id=m.application_id LEFT JOIN campaigns c ON c.id=m.campaign_id WHERE m.status IN('queued','retry_wait') AND m.available_at<=now() AND m.expires_at>now() AND NOT a.paused AND a.next_send_at<=now() AND (m.campaign_id IS NULL OR c.state='sending') ORDER BY CASE WHEN $1::bigint%5=4 AND m.class='bulk' THEN -1 ELSE CASE m.class WHEN 'otp' THEN 0 WHEN 'transactional' THEN 1 ELSE 2 END END,a.next_send_at,m.available_at,m.created_at FOR UPDATE OF m SKIP LOCKED LIMIT 1`, dispatchCounter).Scan(&d.ID, &d.AppID, &d.Recipient, &d.From, &d.FromName, &d.Reply, &d.Subject, &d.HTML, &d.Plain, &d.Attempts, &d.Route, &d.RelayID, &d.FallbackRelayID, &d.CC)
	if e == pgx.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var appRate int
	if e = tx.QueryRow(ctx, "SELECT rate_per_minute FROM applications WHERE id=$1 FOR UPDATE", d.AppID).Scan(&appRate); e != nil {
		return nil, e
	}
	d.Lease = newID("lease_")
	if _, e = tx.Exec(ctx, "UPDATE messages SET status='sending',attempts=attempts+1,route_attempts=route_attempts+1,lease_until=now()+interval '90 seconds',lease_token=$2,updated_at=now() WHERE id=$1", d.ID, d.Lease); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE applications SET next_send_at=now()+$2::interval WHERE id=$1", d.AppID, fmt.Sprintf("%f seconds", 60.0/float64(appRate))); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE platform_settings SET dispatch_counter=dispatch_counter+1,next_send_at=now()+$1::interval WHERE id=1", fmt.Sprintf("%f seconds", 60.0/float64(rate))); e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return d, nil
}
func (a *App) Work(ctx context.Context) error {
	if e := a.Seed(ctx, false); e != nil {
		return e
	}
	go a.pmgLoop(ctx)
	go a.webhookLoop(ctx)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	maintenance := time.NewTicker(30 * time.Second)
	defer maintenance.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-maintenance.C:
			a.maintenance(ctx)
		case <-ticker.C:
			d, e := a.claim(ctx)
			if e != nil {
				slog.Error("queue claim", "error", e)
				continue
			}
			if d == nil {
				continue
			}
			status, detail := a.smtpSend(ctx, *d)
			if e = a.finish(ctx, *d, status, detail); e != nil {
				slog.Error("delivery outcome persistence failed", "message_id", d.ID, "error", e)
			}
		}
	}
}
func (a *App) finish(ctx context.Context, d delivery, status, detail string) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	delay := time.Minute * time.Duration(1<<min(d.Attempts, 6))
	if status == "retry_wait" && d.Attempts >= 7 {
		status = "failed"
	}
	tag, e := tx.Exec(ctx, `UPDATE messages SET status=$3,last_error=$4,failure_category=$6,lease_until=NULL,lease_token=NULL,available_at=now()+$5::interval,relay_accepted_at=CASE WHEN $3='relay_accepted' THEN now() ELSE relay_accepted_at END,updated_at=now() WHERE id=$1 AND lease_token=$2 AND status='sending'`, d.ID, d.Lease, status, detail, delay.String(), classifyFailure(detail))
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("delivery lease no longer belongs to worker")
	}
	if _, e = tx.Exec(ctx, "INSERT INTO delivery_events(message_id,status,detail) VALUES($1,$2,$3)", d.ID, status, detail); e != nil {
		return e
	}
	if status == "failed" {
		if e = a.queueFallback(ctx, tx, d.ID); e != nil {
			return e
		}
	}
	if status == "retry_wait" {
		_, e = tx.Exec(ctx, "UPDATE platform_settings SET consecutive_failures=consecutive_failures+1,circuit_until=CASE WHEN consecutive_failures>=9 THEN now()+interval '2 minutes' ELSE circuit_until END WHERE id=1")
	} else if status == "relay_accepted" {
		_, e = tx.Exec(ctx, "UPDATE platform_settings SET consecutive_failures=0,circuit_until=NULL WHERE id=1")
	}
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) maintenance(ctx context.Context) {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `WITH changed AS(UPDATE messages SET status=CASE WHEN status='sending' THEN 'unknown' ELSE 'expired' END,last_error=CASE WHEN status='sending' THEN 'Worker lease expired; verify gateway logs before resending' ELSE 'Message expired before dispatch' END,lease_until=NULL,lease_token=NULL,updated_at=now() WHERE (status='sending' AND lease_until<now()) OR (status IN('queued','retry_wait') AND expires_at<=now()) RETURNING id,status,last_error) INSERT INTO delivery_events(message_id,status,detail) SELECT id,status,last_error FROM changed`)
	if e != nil {
		return
	}
	campaigns, err := tx.Query(ctx, "SELECT id FROM campaigns WHERE state='sending' FOR UPDATE SKIP LOCKED")
	if err != nil {
		return
	}
	var campaignIDs []string
	for campaigns.Next() {
		var id string
		if campaigns.Scan(&id) != nil {
			campaigns.Close()
			return
		}
		campaignIDs = append(campaignIDs, id)
	}
	campaigns.Close()
	for _, id := range campaignIDs {
		if _, err = tx.Exec(ctx, "UPDATE campaigns SET state='completed' WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM messages WHERE campaign_id=$1 AND status IN('queued','retry_wait','sending'))", id); err != nil {
			return
		}
	}
	tx.Exec(ctx, "DELETE FROM sessions WHERE expires_at<now()")
	tx.Exec(ctx, "DELETE FROM oauth_states WHERE expires_at<now()")
	tx.Exec(ctx, "DELETE FROM login_attempts WHERE expires_at<now()")
	tx.Exec(ctx, "UPDATE messages SET html='',plain='' WHERE created_at<now()-$1::interval AND status NOT IN('queued','retry_wait','sending')", fmt.Sprintf("%d days", a.C.RetentionDays))
	tx.Exec(ctx, "DELETE FROM webhook_jobs WHERE status NOT IN('queued','sending') AND created_at<now()-$1::interval", fmt.Sprintf("%d days", a.C.RetentionDays))
	tx.Commit(ctx)
}
func makeMIME(d delivery, publicURL string) []byte {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	domain := "edp.local"
	if u := strings.TrimPrefix(strings.TrimPrefix(publicURL, "https://"), "http://"); u != "" {
		domain = strings.Split(strings.Split(u, "/")[0], ":")[0]
	}
	from := d.From
	if validDisplayName(d.FromName) && d.FromName != "" {
		from = (&mail.Address{Name: d.FromName, Address: d.From}).String()
		if strings.HasPrefix(from, "=?") {
			from = strings.ReplaceAll(from, "?= =?", "?=\r\n =?")
		}
	}
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nX-EDP-Message-ID: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=%q\r\n", from, d.Recipient, mime.QEncoding.Encode("utf-8", d.Subject), time.Now().Format(time.RFC1123Z), d.ID, domain, d.ID, mw.Boundary())
	if d.Reply != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", d.Reply)
	}
	if len(d.CC) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(d.CC, ",\r\n "))
	}
	b.WriteString("\r\n")
	for _, part := range []struct{ Type, Body string }{{"text/plain", d.Plain}, {"text/html", emailDocument(d.HTML, publicURL)}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", part.Type+"; charset=utf-8")
		h.Set("Content-Transfer-Encoding", "base64")
		p, _ := mw.CreatePart(h)
		encoded := base64.StdEncoding.EncodeToString([]byte(part.Body))
		for len(encoded) > 76 {
			fmt.Fprintf(p, "%s\r\n", encoded[:76])
			encoded = encoded[76:]
		}
		fmt.Fprintf(p, "%s\r\n", encoded)
	}
	mw.Close()
	return b.Bytes()
}
func smtpFailure(e error, uncertain bool) (string, string) {
	if uncertain {
		return "unknown", "SMTP response was lost after message data; verify gateway before resending"
	}
	var response *textproto.Error
	if errors.As(e, &response) && response.Code >= 500 {
		return "failed", fmt.Sprintf("SMTP %d: %.450s", response.Code, response.Msg)
	}
	return "retry_wait", "SMTP connection or temporary rejection; retry scheduled"
}
func (a *App) smtpSend(ctx context.Context, d delivery) (string, string) {
	cc, err := normalizeCC(d.Recipient, d.CC)
	if err != nil {
		return "failed", err.Error()
	}
	d.CC = cc
	config, configErr := a.runtimeConfig(ctx)
	if configErr != nil {
		return "retry_wait", "Runtime configuration is unavailable"
	}
	liveRelay, routeError := a.senderRelay(ctx, d.From)
	if routeError != nil {
		return "failed", routeError.Error()
	}
	relay := liveRelay
	if d.Route == "secondary" {
		relay = d.FallbackRelayID
	}
	if relay != "" {
		var err error
		config, err = a.relayConfig(ctx, relay, d.From, config)
		if err != nil {
			return "failed", err.Error()
		}

	} else if d.Route == "secondary" {
		if liveRelay != "" {
			return "failed", "Sender domain requires its configured relay fallback"
		}
		if config.SecondarySMTPAddress == "" {
			return "failed", "Secondary SMTP relay is not configured"
		}
		config.SMTPAddress = config.SecondarySMTPAddress
		config.SMTPUser = config.SecondarySMTPUser
		config.SMTPPassword = config.SecondarySMTPPassword
		config.SMTPTLS = config.SecondarySMTPTLS
	}

	if d.Route != "secondary" {
		if _, err := a.DB.Exec(ctx, "UPDATE messages SET relay_id=NULLIF($3,'') WHERE id=$1 AND lease_token=$2 AND status='sending'", d.ID, d.Lease, relay); err != nil {
			return "retry_wait", "Could not persist relay selection"
		}
	}

	host, _, e := net.SplitHostPort(config.SMTPAddress)
	if e != nil {
		return "failed", "invalid SMTP address"
	}
	dial := net.Dialer{Timeout: 10 * time.Second}
	conn, e := dial.DialContext(ctx, "tcp", config.SMTPAddress)
	if e != nil {
		return smtpFailure(e, false)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	if config.SMTPTLS == "tls" {
		secure := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if e = secure.HandshakeContext(ctx); e != nil {
			return smtpFailure(e, false)
		}
		conn = secure
	}
	client, e := smtp.NewClient(conn, host)
	if e != nil {
		return smtpFailure(e, false)
	}
	defer client.Close()
	if config.SMTPTLS == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return "failed", "SMTP gateway does not offer required STARTTLS"
		}
		if e = client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); e != nil {
			return smtpFailure(e, false)
		}
	}
	if config.SMTPUser != "" {
		if e = client.Auth(smtp.PlainAuth("", config.SMTPUser, config.SMTPPassword, host)); e != nil {
			return smtpFailure(e, false)
		}
	}
	domain, domainError := senderDomain(d.From)
	if domainError != nil {
		return "failed", domainError.Error()
	}
	// Recheck suspension immediately before crossing the SMTP boundary.
	var allowed bool
	e = a.DB.QueryRow(ctx, `SELECT NOT p.paused AND NOT a.paused AND m.status='sending' AND m.lease_token=$2 AND m.expires_at>now() AND (m.campaign_id IS NULL OR c.state='sending') AND (m.route<>'secondary' OR a.fallback_enabled) AND (m.route<>'secondary' OR coalesce(m.relay_id,'')=coalesce((SELECT relay_id FROM sender_domain_routes WHERE domain=$3),'')) AND (m.route<>'primary' OR $4=coalesce((SELECT relay_id FROM sender_domain_routes WHERE domain=$3),'')) AND (m.route<>'secondary' OR m.relay_id IS NULL OR EXISTS(SELECT 1 FROM smtp_relays s WHERE s.id=m.relay_id AND s.enabled AND $3=ANY(s.allowed_domains) AND s.fallback_id=$4)) AND (NOT p.strict_sender_domains OR EXISTS(SELECT 1 FROM sender_domain_routes dr JOIN smtp_relays sr ON sr.id=dr.relay_id WHERE dr.domain=$3 AND sr.enabled AND $3=ANY(sr.allowed_domains))) AND ($4='' OR EXISTS(SELECT 1 FROM smtp_relays sr WHERE sr.id=$4 AND sr.enabled AND $3=ANY(sr.allowed_domains))) FROM messages m JOIN applications a ON a.id=m.application_id CROSS JOIN platform_settings p LEFT JOIN campaigns c ON c.id=m.campaign_id WHERE m.id=$1 AND p.id=1`, d.ID, d.Lease, domain, relay).Scan(&allowed)
	if e != nil || !allowed {
		return "retry_wait", "Dispatch paused, expired, or relay routing changed before SMTP submission"
	}
	envelope := d.From
	if config.BounceDomain != "" {
		envelope = "bounce+" + d.ID + "@" + config.BounceDomain
	}
	if _, e = a.DB.Exec(ctx, "UPDATE messages SET envelope_sender=$3 WHERE id=$1 AND lease_token=$2 AND status='sending'", d.ID, d.Lease, envelope); e != nil {
		return "retry_wait", "Could not persist the SMTP envelope before submission"
	}
	if e = client.Mail(envelope); e != nil {
		return smtpFailure(e, false)
	}
	if e = client.Rcpt(d.Recipient); e != nil {
		return smtpFailure(e, false)
	}
	for _, address := range d.CC {
		if e = client.Rcpt(address); e != nil {
			// DATA is never submitted unless all recipients were accepted.
			return smtpFailure(e, false)
		}
	}
	writer, e := client.Data()
	if e != nil {
		return smtpFailure(e, false)
	}
	if _, e = writer.Write(makeMIME(d, config.PublicURL)); e != nil {
		return smtpFailure(e, true)
	}
	if e = writer.Close(); e != nil {
		var response *textproto.Error
		if errors.As(e, &response) {
			return smtpFailure(e, false)
		}
		return smtpFailure(e, true)
	}
	client.Quit()
	return "relay_accepted", "Accepted by SMTP gateway"
}
