package edp

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"regexp"
	"strings"
)

var permanentReply = regexp.MustCompile(`\b5[0-9]{2}\b|\b5\.[0-9]\.[0-9]+\b`)
var temporaryReply = regexp.MustCompile(`\b4[0-9]{2}\b|\b4\.[0-9]\.[0-9]+\b`)
var ipBlock = regexp.MustCompile(`(?i)spamhaus|spamcop|barracuda|blacklist|blacklisted|ip.{0,100}(?:block|reputation)|(?:block|reputation).{0,100}ip`)

func classifyFailure(detail string) string {
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "5.1.1") || strings.Contains(lower, "user unknown") || strings.Contains(lower, "no such user") || strings.Contains(lower, "invalid recipient") {
		return "invalid_recipient"
	}
	if temporaryReply.MatchString(detail) && !permanentReply.MatchString(detail) {
		return "temporary"
	}
	if strings.Contains(lower, "malware") || strings.Contains(lower, "virus") || strings.Contains(lower, "quarantine") || strings.Contains(lower, "spam content") {
		return "content_policy"
	}
	if permanentReply.MatchString(detail) && ipBlock.MatchString(detail) {
		return "ip_reputation"
	}
	return "other"
}

// A secondary relay is used at most once, only after a confirmed permanent
// transport rejection. PMG deferral, quarantine, or an uncertain send never qualifies.
func (a *App) queueFallback(ctx context.Context, tx pgx.Tx, id string) error {
	config, e := readRuntimeConfig(ctx, tx, a.C)
	if e != nil {
		return e
	}
	if config.SecondarySMTPAddress == "" {
		return nil
	}
	var campaignID *string
	var campaignState string
	if e := tx.QueryRow(ctx, "SELECT campaign_id FROM messages WHERE id=$1", id).Scan(&campaignID); e != nil {
		return e
	}
	if campaignID != nil {
		if e := tx.QueryRow(ctx, "SELECT state FROM campaigns WHERE id=$1 FOR UPDATE", *campaignID).Scan(&campaignState); e != nil {
			return e
		}
		if !contains([]string{"sending", "completed", "paused"}, campaignState) {
			return nil
		}
	}

	tag, e := tx.Exec(ctx, `UPDATE messages m SET route='secondary',secondary_used=true,status='queued',route_attempts=0,available_at=now(),lease_until=NULL,lease_token=NULL,relay_accepted_at=NULL,updated_at=now() FROM applications a WHERE m.id=$1 AND m.application_id=a.id AND a.fallback_enabled AND m.route='primary' AND NOT m.secondary_used AND m.failure_category='ip_reputation' AND m.status IN('failed','bounced') AND m.expires_at>now()`, id)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 1 {
		if campaignID != nil && campaignState == "completed" {
			if _, e = tx.Exec(ctx, "UPDATE campaigns SET state='sending' WHERE id=$1", *campaignID); e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, "INSERT INTO delivery_events(message_id,status,detail) VALUES($1,'fallback_queued','Confirmed permanent reputation rejection; queued for the secondary SMTP relay')", id)
	}
	return e
}
func (a *App) applyGatewayOutcome(ctx context.Context, id, eventKey, status, detail, category string) error {
	if !contains([]string{"remote_accepted", "bounced", "remote_deferred", "relay_accepted"}, status) {
		return fmt.Errorf("unsupported gateway outcome")
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var route, state string
	e = tx.QueryRow(ctx, "SELECT route,status FROM messages WHERE id=$1 FOR UPDATE", id).Scan(&route, &state)
	if e != nil {
		return e
	}
	if route != "primary" || !contains([]string{"relay_accepted", "unknown", "remote_accepted"}, state) {
		return nil
	}
	var exists bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM delivery_events WHERE event_key=$1)", eventKey).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return nil
	}
	if len(detail) > 512 {
		detail = detail[:512]
	}
	if category == "ip_reputation" && classifyFailure(detail) != "ip_reputation" {
		category = "other"
	}
	newStatus := status
	if status == "remote_deferred" {
		newStatus = "relay_accepted"
	}
	_, e = tx.Exec(ctx, `UPDATE messages SET status=$2,gateway_status=$3,failure_category=$4,last_error=CASE WHEN $3 IN('bounced','remote_deferred') THEN $5 ELSE '' END,relay_accepted_at=coalesce(relay_accepted_at,now()),updated_at=now() WHERE id=$1`, id, newStatus, status, category, detail)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO delivery_events(message_id,status,detail,event_key) VALUES($1,$2,$3,$4)", id, status, detail, eventKey); e != nil {
		return e
	}
	if status == "bounced" {
		if e = a.queueFallback(ctx, tx, id); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
