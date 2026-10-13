package edp

import (
	"context"
	"fmt"
	"net"
	"net/mail"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type RelayInput struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Address        string   `json:"address"`
	Username       string   `json:"username"`
	Password       string   `json:"password"`
	TLS            string   `json:"tls"`
	BounceDomain   string   `json:"bounce_domain"`
	AllowedDomains []string `json:"allowed_domains"`
	Enabled        bool     `json:"enabled"`
	FallbackID     string   `json:"fallback_id"`
	ClearPassword  bool     `json:"clear_password"`
}

func relayDomain(value string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(value))
	if domain == "" || len(domain) > 253 || net.ParseIP(domain) != nil {
		return "", fmt.Errorf("use an exact ASCII sender domain without wildcards")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid sender domain")
		}
		for _, c := range label {
			if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return "", fmt.Errorf("use an exact ASCII sender domain; encode international domains as punycode")
			}
		}
	}
	return domain, nil
}
func senderDomain(from string) (string, error) {
	address, e := mail.ParseAddress(from)
	if e != nil {
		return "", fmt.Errorf("invalid sender address")
	}
	separator := strings.LastIndexByte(address.Address, '@')
	if separator < 0 {
		return "", fmt.Errorf("invalid sender address")
	}
	return relayDomain(address.Address[separator+1:])
}
func relayAllows(domains []string, domain string) bool {
	for _, allowed := range domains {
		if allowed == domain {
			return true
		}
	}
	return false
}
func normalizeRelay(in RelayInput, development bool) (RelayInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Address = strings.TrimSpace(in.Address)
	in.Username = strings.TrimSpace(in.Username)
	in.TLS = strings.ToLower(strings.TrimSpace(in.TLS))
	in.BounceDomain = strings.TrimSpace(in.BounceDomain)
	in.FallbackID = strings.TrimSpace(in.FallbackID)
	if in.Name == "" || len(in.Name) > 120 || strings.ContainsAny(in.Name, "\r\n\x00") {
		return in, fmt.Errorf("relay name is required and must be at most 120 characters")
	}
	if len(in.Username) > 512 || len(in.Password) > 4096 || strings.ContainsAny(in.Username+in.Password, "\r\n\x00") {
		return in, fmt.Errorf("invalid SMTP credentials")
	}
	if e := validateSMTP(in.Address, in.TLS, development, false); e != nil {
		return in, e
	}
	host, port, _ := net.SplitHostPort(in.Address)
	if strings.ContainsAny(host, " /\\@?#\t") || strings.ContainsAny(port, " /\\@?#\t") {
		return in, fmt.Errorf("invalid SMTP address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return in, fmt.Errorf("SMTP port must be between 1 and 65535")
	}
	if in.BounceDomain != "" {
		var e error
		in.BounceDomain, e = relayDomain(in.BounceDomain)
		if e != nil {
			return in, fmt.Errorf("invalid bounce domain")
		}
	}
	domains := make([]string, 0, len(in.AllowedDomains))
	seen := map[string]bool{}
	for _, raw := range in.AllowedDomains {
		domain, e := relayDomain(raw)
		if e != nil {
			return in, e
		}
		if !seen[domain] {
			domains = append(domains, domain)
			seen[domain] = true
		}
	}
	if in.ID == "" && len(domains) == 0 {
		return in, fmt.Errorf("add at least one allowed sender domain")
	}
	sort.Strings(domains)
	in.AllowedDomains = domains
	if in.ClearPassword && in.Password != "" {
		return in, fmt.Errorf("enter a password or clear it, not both")
	}
	return in, nil
}
func (a *App) saveRelay(ctx context.Context, in RelayInput) (string, error) {
	var e error
	in, e = normalizeRelay(in, a.C.Dev)
	if e != nil {
		return "", e
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(824193)"); e != nil {
		return "", e
	}
	password := ""
	if in.ID == "" {
		in.ID = newID("relay_")
	} else {
		if e = tx.QueryRow(ctx, "SELECT password FROM smtp_relays WHERE id=$1 FOR UPDATE", in.ID).Scan(&password); e != nil {
			if e == pgx.ErrNoRows {
				return "", fmt.Errorf("relay not found")
			}
			return "", e
		}
	}
	if in.FallbackID != "" {
		next := in.FallbackID
		seen := map[string]bool{in.ID: true}
		for next != "" {
			if seen[next] {
				return "", fmt.Errorf("relay fallback must not form a cycle")
			}
			seen[next] = true
			if e = tx.QueryRow(ctx, "SELECT COALESCE(fallback_id,'') FROM smtp_relays WHERE id=$1", next).Scan(&next); e != nil {
				if e == pgx.ErrNoRows {
					return "", fmt.Errorf("fallback relay not found")
				}
				return "", e
			}
		}
	}
	if in.ClearPassword {
		password = ""
	} else if in.Password != "" {
		password = seal(a.C.Secret, in.Password)
	}
	_, e = tx.Exec(ctx, `INSERT INTO smtp_relays(id,name,address,username,password,tls,bounce_domain,allowed_domains,enabled,fallback_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')) ON CONFLICT(id) DO UPDATE SET name=excluded.name,address=excluded.address,username=excluded.username,password=excluded.password,tls=excluded.tls,bounce_domain=excluded.bounce_domain,allowed_domains=excluded.allowed_domains,enabled=excluded.enabled,fallback_id=excluded.fallback_id`, in.ID, in.Name, in.Address, in.Username, password, in.TLS, in.BounceDomain, in.AllowedDomains, in.Enabled, in.FallbackID)
	if e != nil {
		return "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", e
	}
	return in.ID, nil
}
func (a *App) listRelays(ctx context.Context) ([]map[string]any, error) {
	rows, e := a.DB.Query(ctx, `SELECT id,name,address,username,password<>'',tls,bounce_domain,allowed_domains,enabled,COALESCE(fallback_id,'') FROM smtp_relays ORDER BY name,id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, name, address, user, mode, bounce, fallback string
		var hasPassword, enabled bool
		var domains []string
		if e = rows.Scan(&id, &name, &address, &user, &hasPassword, &mode, &bounce, &domains, &enabled, &fallback); e != nil {
			return nil, e
		}
		result = append(result, map[string]any{"id": id, "name": name, "address": address, "username": user, "password_set": hasPassword, "tls": mode, "bounce_domain": bounce, "allowed_domains": domains, "enabled": enabled, "fallback_id": fallback})
	}
	return result, rows.Err()
}
func (a *App) setDomainRoute(ctx context.Context, domain, relayID string) error {
	domain, e := relayDomain(domain)
	if e != nil {
		return e
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(824193)"); e != nil {
		return e
	}
	if relayID == "" {
		_, e = tx.Exec(ctx, "DELETE FROM sender_domain_routes WHERE domain=$1", domain)
	} else {
		var domains []string
		var enabled bool
		if e = tx.QueryRow(ctx, "SELECT allowed_domains,enabled FROM smtp_relays WHERE id=$1", relayID).Scan(&domains, &enabled); e != nil {
			if e == pgx.ErrNoRows {
				return fmt.Errorf("relay not found")
			}
			return e
		}
		if !enabled || !relayAllows(domains, domain) {
			return fmt.Errorf("relay is disabled or does not allow this sender domain")
		}
		_, e = tx.Exec(ctx, "INSERT INTO sender_domain_routes(domain,relay_id) VALUES($1,$2) ON CONFLICT(domain) DO UPDATE SET relay_id=excluded.relay_id", domain, relayID)
	}
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) senderRelay(ctx context.Context, from string) (string, error) {
	return senderRelayWith(ctx, a.DB, from)
}
func senderRelayWith(ctx context.Context, q rowQuerier, from string) (string, error) {
	domain, e := senderDomain(from)
	if e != nil {
		return "", e
	}
	var id string
	var strict, namedOnly bool
	var domains []string
	var enabled bool
	e = q.QueryRow(ctx, `SELECT p.strict_sender_domains,p.named_relays_only,COALESCE(r.id,''),COALESCE(r.allowed_domains,'{}'::text[]),COALESCE(r.enabled,false) FROM platform_settings p LEFT JOIN sender_domain_routes d ON d.domain=$1 LEFT JOIN smtp_relays r ON r.id=d.relay_id WHERE p.id=1`, domain).Scan(&strict, &namedOnly, &id, &domains, &enabled)
	if e != nil {
		return "", e
	}
	if id == "" {
		if strict || namedOnly {
			return "", fmt.Errorf("sender domain has no configured relay route")
		}
		return "", nil
	}
	if !enabled || !relayAllows(domains, domain) {
		return "", fmt.Errorf("sender domain route is disabled or not allowed by its relay")
	}
	return id, nil
}
func (a *App) relayConfig(ctx context.Context, id, from string, c Config) (Config, error) {
	domain, e := senderDomain(from)
	if e != nil {
		return c, e
	}
	var password string
	var enabled bool
	var domains []string
	e = a.DB.QueryRow(ctx, "SELECT address,username,password,tls,bounce_domain,allowed_domains,enabled FROM smtp_relays WHERE id=$1", id).Scan(&c.SMTPAddress, &c.SMTPUser, &password, &c.SMTPTLS, &c.BounceDomain, &domains, &enabled)
	if e != nil {
		if e == pgx.ErrNoRows {
			return c, fmt.Errorf("relay not found")
		}
		return c, e
	}
	if !enabled || !relayAllows(domains, domain) {
		return c, fmt.Errorf("relay is disabled or does not allow the sender domain")
	}
	if e = validateSMTP(c.SMTPAddress, c.SMTPTLS, c.Dev, false); e != nil {
		return c, e
	}
	c.SMTPPassword, e = unseal(a.C.Secret, password)
	if e != nil {
		return c, fmt.Errorf("relay credential could not be decrypted")
	}
	return c, nil
}
func (a *App) fallbackRelay(ctx context.Context, from, relayID string) (string, error) {
	var fallback string
	e := a.DB.QueryRow(ctx, "SELECT COALESCE(fallback_id,'') FROM smtp_relays WHERE id=$1", relayID).Scan(&fallback)
	if e != nil {
		if e == pgx.ErrNoRows {
			return "", fmt.Errorf("relay not found")
		}
		return "", e
	}
	if fallback == "" {
		return "", nil
	}
	if fallback == relayID {
		return "", fmt.Errorf("relay cannot fall back to itself")
	}
	if _, e = a.relayConfig(ctx, relayID, from, a.C); e != nil {
		return "", e
	}
	if _, e = a.relayConfig(ctx, fallback, from, a.C); e != nil {
		return "", e
	}
	return fallback, nil
}
func (a *App) testRelay(ctx context.Context, in RelayInput) error {
	in, e := normalizeRelay(in, a.C.Dev)
	if e != nil {
		return e
	}
	password := in.Password
	if in.ID != "" && in.Password == "" && !in.ClearPassword {
		var encrypted string
		if e = a.DB.QueryRow(ctx, "SELECT password FROM smtp_relays WHERE id=$1", in.ID).Scan(&encrypted); e != nil {
			if e == pgx.ErrNoRows {
				return fmt.Errorf("relay not found")
			}
			return e
		}
		password, e = unseal(a.C.Secret, encrypted)
		if e != nil {
			return fmt.Errorf("relay credential could not be decrypted")
		}
	}
	if in.ClearPassword {
		password = ""
	}
	c := a.C
	c.SMTPAddress = in.Address
	c.SMTPUser = in.Username
	c.SMTPPassword = password
	c.SMTPTLS = in.TLS
	return testSMTP(ctx, c, false)
}
