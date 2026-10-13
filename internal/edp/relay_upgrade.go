package edp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

const importedPrimaryRelay = "relay_imported_primary"
const importedSecondaryRelay = "relay_imported_secondary"

// Run exactly once inside migration 013's transaction. Saved UI settings take
// precedence over bootstrap environment values; encrypted credentials stay sealed.
func (a *App) migrateLegacyRelays(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(824193)"); err != nil {
		return err
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT runtime_config FROM platform_settings WHERE id=1 FOR UPDATE").Scan(&raw); err != nil {
		return err
	}
	var saved RuntimeSettings
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	primary := saved.SMTP
	if primary == nil {
		primary = &SMTPSettings{a.C.SMTPAddress, a.C.SMTPTLS, a.C.SMTPUser, seal(a.C.Secret, a.C.SMTPPassword), a.C.BounceDomain}
	}
	secondary := saved.Secondary
	if secondary == nil {
		secondary = &SMTPSettings{Address: a.C.SecondarySMTPAddress, TLS: a.C.SecondarySMTPTLS, Username: a.C.SecondarySMTPUser, PasswordEnc: seal(a.C.Secret, a.C.SecondarySMTPPassword)}
	}
	// The legacy fallback inherited the primary bounce domain.
	secondary.BounceDomain = primary.BounceDomain
	rows, err := tx.Query(ctx, `SELECT from_address FROM applications UNION SELECT from_address FROM messages WHERE relay_id IS NULL`)
	if err != nil {
		return err
	}
	domains := map[string]bool{}
	for rows.Next() {
		var from string
		if err = rows.Scan(&from); err != nil {
			rows.Close()
			return err
		}
		domain, err := senderDomain(from)
		if err != nil {
			rows.Close()
			return fmt.Errorf("legacy relay upgrade: invalid stored sender domain")
		}
		domains[domain] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	allowed := []string{}
	for domain := range domains {
		allowed = append(allowed, domain)
	}
	sort.Strings(allowed)
	insert := func(id, name string, config *SMTPSettings) error {
		if config.Address == "" {
			return nil
		}
		if err := validateSMTP(config.Address, config.TLS, a.C.Dev, false); err != nil {
			return fmt.Errorf("legacy relay upgrade: %w", err)
		}
		// Do not overwrite a colliding ID or any already configured named relay.
		_, err := tx.Exec(ctx, `INSERT INTO smtp_relays(id,name,address,username,password,tls,bounce_domain,allowed_domains) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, name, config.Address, config.Username, config.PasswordEnc, config.TLS, config.BounceDomain, allowed)
		return err
	}
	if err = insert(importedSecondaryRelay, "Imported secondary relay", secondary); err != nil {
		return err
	}
	if err = insert(importedPrimaryRelay, "Imported primary relay", primary); err != nil {
		return err
	}
	if primary.Address != "" {
		if secondary.Address != "" {
			if _, err = tx.Exec(ctx, "UPDATE smtp_relays SET fallback_id=$2 WHERE id=$1", importedPrimaryRelay, importedSecondaryRelay); err != nil {
				return err
			}
		}
		for _, domain := range allowed {
			if _, err = tx.Exec(ctx, "INSERT INTO sender_domain_routes(domain,relay_id) VALUES($1,$2) ON CONFLICT(domain) DO NOTHING", domain, importedPrimaryRelay); err != nil {
				return err
			}
		}
		// Retain original SMTP bodies, leases, acceptance state and scheduling.
		if _, err = tx.Exec(ctx, `UPDATE messages SET relay_id=$1 WHERE relay_id IS NULL`, importedPrimaryRelay); err != nil {
			return err
		}
	}
	if secondary.Address != "" {
		if _, err = tx.Exec(ctx, `UPDATE messages SET fallback_relay_id=$1 WHERE route='secondary' AND fallback_relay_id IS NULL`, importedSecondaryRelay); err != nil {
			return err
		}
	}
	backup, _ := json.Marshal(map[string]any{"smtp": primary, "secondary": secondary})
	_, err = tx.Exec(ctx, `UPDATE platform_settings SET legacy_relay_backup=$1,runtime_config=runtime_config-'smtp'-'secondary',strict_sender_domains=true WHERE id=1`, backup)
	return err
}

func seedDemoRelay(ctx context.Context, tx pgx.Tx) error {
	var migrated bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=13)").Scan(&migrated); err != nil {
		return err
	}
	if !migrated {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM smtp_relays WHERE id=$1)", importedPrimaryRelay).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var from string
	if err := tx.QueryRow(ctx, "SELECT from_address FROM applications WHERE id='app_demo'").Scan(&from); err != nil {
		return err
	}
	domain, err := senderDomain(from)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE smtp_relays SET allowed_domains=array_append(allowed_domains,$1) WHERE id IN($2,$3) AND NOT $1=ANY(allowed_domains)`, domain, importedPrimaryRelay, importedSecondaryRelay); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO sender_domain_routes(domain,relay_id) VALUES($1,$2) ON CONFLICT(domain) DO NOTHING", domain, importedPrimaryRelay)
	return err
}
