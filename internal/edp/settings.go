package edp

import (
	"context"
	"fmt"
	"github.com/coreos/go-oidc/v3/oidc"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func runtimeView(c Config) map[string]any {
	return map[string]any{"SMTPAddress": c.SMTPAddress, "SMTPTLS": c.SMTPTLS, "SMTPUser": c.SMTPUser, "SMTPSecretSet": c.SMTPPassword != "", "BounceDomain": c.BounceDomain, "SecondaryAddress": c.SecondarySMTPAddress, "SecondaryTLS": c.SecondarySMTPTLS, "SecondaryUser": c.SecondarySMTPUser, "SecondarySecretSet": c.SecondarySMTPPassword != "", "PMGURL": c.PMGURL, "PMGUser": c.PMGUser, "PMGSecretSet": c.PMGPassword != "", "PMGNodes": strings.Join(c.PMGNodes, ", "), "PMGCAPEM": c.PMGCAPEM, "Issuer": c.Issuer, "ProviderName": c.ProviderName, "ClientID": c.ClientID, "OIDCSecretSet": c.ClientSecret != "", "OIDCEnabled": c.ssoEnabled(), "Scopes": strings.Join(c.OIDCScopes, " "), "GroupsClaim": c.GroupsClaim, "PublicURL": c.PublicURL}
}
func keepSecret(r *http.Request, name, old string) string {
	if r.FormValue(name+"_clear") == "on" {
		return ""
	}
	if v := r.FormValue(name); v != "" {
		return v
	}
	return old
}
func (a *App) connectionCandidate(r *http.Request, group string, c Config) (Config, error) {
	switch group {
	case "smtp":
		c.SMTPAddress = strings.TrimSpace(r.FormValue("address"))
		c.SMTPTLS = r.FormValue("tls")
		c.SMTPUser = strings.TrimSpace(r.FormValue("username"))
		c.SMTPPassword = keepSecret(r, "password", c.SMTPPassword)
		c.BounceDomain = strings.TrimSpace(r.FormValue("bounce_domain"))
		if strings.ContainsAny(c.BounceDomain, "/\r\n @") {
			return c, fmt.Errorf("bounce domain must be a domain name")
		}
		if e := validateSMTP(c.SMTPAddress, c.SMTPTLS, c.Dev, false); e != nil {
			return c, e
		}
	case "secondary":
		c.SecondarySMTPAddress = strings.TrimSpace(r.FormValue("address"))
		c.SecondarySMTPTLS = r.FormValue("tls")
		c.SecondarySMTPUser = strings.TrimSpace(r.FormValue("username"))
		c.SecondarySMTPPassword = keepSecret(r, "password", c.SecondarySMTPPassword)
		if e := validateSMTP(c.SecondarySMTPAddress, c.SecondarySMTPTLS, c.Dev, true); e != nil {
			return c, e
		}
	case "pmg":
		c.PMGURL = strings.TrimRight(strings.TrimSpace(r.FormValue("url")), "/")
		c.PMGUser = strings.TrimSpace(r.FormValue("username"))
		c.PMGPassword = keepSecret(r, "password", c.PMGPassword)
		c.PMGNodes = []string{}
		for _, n := range strings.Split(r.FormValue("nodes"), ",") {
			if n = strings.TrimSpace(n); n != "" {
				c.PMGNodes = append(c.PMGNodes, n)
			}
		}
		if len(c.PMGNodes) > 20 {
			return c, fmt.Errorf("configure at most twenty gateway nodes")
		}
		c.PMGCAPEM = strings.TrimSpace(r.FormValue("ca_pem"))
		c.PMGCAFile = ""
		if c.PMGURL != "" {
			if e := validateIssuer(c.PMGURL, c.Dev); e != nil {
				return c, e
			}
			if c.PMGUser == "" || c.PMGPassword == "" {
				return c, fmt.Errorf("PMG username and password are required")
			}
		}
		if e := validateCAPEM(c.PMGCAPEM); e != nil {
			return c, e
		}
	case "oidc":
		c.Issuer = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(r.FormValue("issuer")), "/"), "/.well-known/openid-configuration")
		c.ProviderName = strings.TrimSpace(r.FormValue("provider_name"))
		c.ClientID = strings.TrimSpace(r.FormValue("client_id"))
		c.ClientSecret = keepSecret(r, "client_secret", c.ClientSecret)
		c.PublicURL = strings.TrimRight(strings.TrimSpace(r.FormValue("public_url")), "/")
		c.OIDCScopes = strings.Fields(r.FormValue("scopes"))
		c.GroupsClaim = strings.TrimSpace(r.FormValue("groups_claim"))
		if c.ProviderName == "" {
			c.ProviderName = "Single sign-on"
		}
		if c.GroupsClaim == "" {
			c.GroupsClaim = "groups"
		}
		if e := validateIssuer(c.PublicURL, c.Dev); e != nil {
			return c, e
		}
		c.OIDCDisabled = r.FormValue("enabled") != "on"
		if !c.OIDCDisabled {
			if c.ClientID == "" {
				return c, fmt.Errorf("client ID is required to enable SSO")
			}
			if e := validateIssuer(c.Issuer, c.Dev); e != nil {
				return c, e
			}
			if !contains(c.OIDCScopes, "openid") {
				return c, fmt.Errorf("SSO scopes must include openid")
			}
		}
		if c.Issuer != "" {
			if e := validateIssuer(c.Issuer, c.Dev); e != nil {
				return c, e
			}
		}
	default:
		return c, fmt.Errorf("unknown configuration section")
	}
	return c, nil
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	if !a.require(w, r, "*", "platform.manage") {
		return
	}
	c, e := a.runtimeConfig(r.Context())
	if e != nil {
		problem(w, 500, "runtime configuration unavailable")
		return
	}
	tab := r.URL.Query().Get("tab")
	if !contains([]string{"limits", "smtp", "pmg", "oidc", "account"}, tab) {
		tab = "limits"
	}
	if r.Method == "POST" {
		if a.relaySettingsAction(w, r) {
			return
		}
		action := r.FormValue("action")
		if action == "" || action == "limits" {
			rate, connections := positiveInt(r.FormValue("rate")), positiveInt(r.FormValue("connections"))
			if rate < 1 || connections < 1 || connections > 100 {
				problem(w, 400, "rate must be positive; connections must be 1–100")
				return
			}
			_, e = a.DB.Exec(r.Context(), "UPDATE platform_settings SET paused=$1,rate_per_minute=$2,max_connections=$3,circuit_until=CASE WHEN $4 THEN NULL ELSE circuit_until END,consecutive_failures=CASE WHEN $4 THEN 0 ELSE consecutive_failures END WHERE id=1", r.FormValue("paused") == "on", rate, connections, r.FormValue("reset_circuit") == "on")
			if e != nil {
				problem(w, 500, "settings update failed")
				return
			}
			a.audit(r.Context(), person(r).ID, "platform.settings", "*", "")
			http.Redirect(w, r, "/settings?tab=limits", 303)
			return
		}
		if action == "account.password" {
			var old string
			e = a.DB.QueryRow(r.Context(), "SELECT password_hash FROM users WHERE id=$1", person(r).ID).Scan(&old)
			password := r.FormValue("new_password")
			if e != nil || !checkPassword(old, r.FormValue("current_password")) {
				problem(w, 400, "current local password is incorrect")
				return
			}
			if len(password) < 12 || password != r.FormValue("repeat_password") {
				problem(w, 400, "new passwords must match and contain at least 12 characters")
				return
			}
			hash, err := hashPassword(password)
			if err != nil {
				problem(w, 500, "password update failed")
				return
			}
			tx, err := a.DB.Begin(r.Context())
			if err != nil {
				problem(w, 500, "password update failed")
				return
			}
			defer tx.Rollback(r.Context())
			_, err = tx.Exec(r.Context(), "UPDATE users SET password_hash=$2 WHERE id=$1", person(r).ID, hash)
			if err == nil {
				cookie, _ := r.Cookie("edp_session")
				current := ""
				if cookie != nil {
					current = tokenHash(cookie.Value)
				}
				_, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2", person(r).ID, current)
			}
			if err == nil {
				err = tx.Commit(r.Context())
			}
			if err != nil {
				problem(w, 500, "password update failed")
				return
			}
			a.audit(r.Context(), person(r).ID, "account.password.changed", "*", "")
			http.Redirect(w, r, "/settings?tab=account&saved=1", 303)
			return
		}
		group, operation, ok := strings.Cut(action, ".")
		if !ok || operation != "save" && operation != "test" {
			problem(w, 400, "unknown configuration action")
			return
		}
		candidate, err := a.connectionCandidate(r, group, c)
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
		if operation == "test" {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			message := ""
			switch group {
			case "smtp", "secondary":
				err = testSMTP(ctx, candidate, group == "secondary")
				message = "SMTP connection verified. No email was submitted."
			case "pmg":
				message, err = a.testPMG(ctx, candidate)
			case "oidc":
				if candidate.Issuer == "" {
					err = fmt.Errorf("enter the issuer or discovery URL")
				} else {
					provider, discoveryErr := oidc.NewProvider(ctx, candidate.Issuer)
					err = discoveryErr
					if err == nil {
						var metadata struct {
							Methods []string `json:"code_challenge_methods_supported"`
						}
						provider.Claims(&metadata)
						message = "OIDC discovery verified. Complete a real sign-in to verify client credentials, callback, and group claims."
					}
				}
			default:
				err = fmt.Errorf("unsupported connection test")
			}
			if err != nil {
				message = err.Error()
				if group == "oidc" {
					message = "OIDC discovery failed; verify issuer URL, connectivity, and TLS trust"
				}
				problem(w, 400, message)
				return
			}
			respond(w, 200, map[string]any{"success": true, "message": message})
			return
		}
		if err = a.saveRuntime(r.Context(), group, candidate); err != nil {
			problem(w, 500, "configuration could not be saved")
			return
		}
		a.audit(r.Context(), person(r).ID, "configuration."+group+".saved", "*", "Secrets encrypted; prior values not logged")
		if group == "secondary" {
			group = "smtp"
		}
		http.Redirect(w, r, "/settings?tab="+group+"&saved=1", 303)
		return
	}
	data, _ := a.one(r.Context(), "SELECT paused,rate_per_minute,max_connections,circuit_until,strict_sender_domains FROM platform_settings WHERE id=1")
	relays, _ := a.listRelays(r.Context())
	routes, _ := a.query(r.Context(), "SELECT d.domain,d.relay_id,r.name FROM sender_domain_routes d JOIN smtp_relays r ON r.id=d.relay_id ORDER BY domain")
	selected := map[string]any{"tls": "starttls", "enabled": true}
	for _, relay := range relays {
		if relay["id"] == r.URL.Query().Get("relay") {
			selected = relay
		}
	}
	a.render(w, r, "settings", map[string]any{"Settings": data, "Relays": relays, "DomainRoutes": routes, "Relay": selected, "Live": runtimeView(c), "SMTP": c.SMTPAddress, "Issuer": c.Issuer, "Retention": c.RetentionDays, "PMG": c.PMGURL, "PMGNodes": strings.Join(c.PMGNodes, ", "), "Secondary": c.SecondarySMTPAddress, "Tab": tab, "Saved": r.URL.Query().Get("saved") == "1"})
}
func positiveInt(s string) int { n, _ := strconv.Atoi(s); return n }
