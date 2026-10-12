package edp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

type SMTPSettings struct {
	Address      string `json:"address"`
	TLS          string `json:"tls"`
	Username     string `json:"username"`
	PasswordEnc  string `json:"password_enc"`
	BounceDomain string `json:"bounce_domain,omitempty"`
}
type PMGSettings struct {
	URL         string   `json:"url"`
	Username    string   `json:"username"`
	PasswordEnc string   `json:"password_enc"`
	Nodes       []string `json:"nodes"`
	CAPEM       string   `json:"ca_pem"`
}
type OIDCSettings struct {
	Enabled         bool     `json:"enabled"`
	Issuer          string   `json:"issuer"`
	ProviderName    string   `json:"provider_name"`
	ClientID        string   `json:"client_id"`
	ClientSecretEnc string   `json:"client_secret_enc"`
	Scopes          []string `json:"scopes"`
	GroupsClaim     string   `json:"groups_claim"`
}
type RuntimeSettings struct {
	SMTP      *SMTPSettings `json:"smtp,omitempty"`
	Secondary *SMTPSettings `json:"secondary,omitempty"`
	PMG       *PMGSettings  `json:"pmg,omitempty"`
	OIDC      *OIDCSettings `json:"oidc,omitempty"`
	PublicURL string        `json:"public_url,omitempty"`
}

func (a *App) runtimeConfig(ctx context.Context) (Config, error) {
	return readRuntimeConfig(ctx, a.DB, a.C)
}
func readRuntimeConfig(ctx context.Context, q rowQuerier, base Config) (Config, error) {
	c := base
	var data []byte
	if e := q.QueryRow(ctx, "SELECT runtime_config FROM platform_settings WHERE id=1").Scan(&data); e != nil {
		return c, e
	}
	var s RuntimeSettings
	if e := json.Unmarshal(data, &s); e != nil {
		return c, e
	}
	decrypt := func(encoded string) (string, error) { return unseal(base.Secret, encoded) }
	if s.PublicURL != "" {
		c.PublicURL = s.PublicURL
	}
	if s.SMTP != nil {
		c.SMTPAddress = s.SMTP.Address
		c.SMTPTLS = s.SMTP.TLS
		c.SMTPUser = s.SMTP.Username
		var e error
		c.SMTPPassword, e = decrypt(s.SMTP.PasswordEnc)
		if e != nil {
			return c, e
		}
		c.BounceDomain = s.SMTP.BounceDomain
	}
	if s.Secondary != nil {
		c.SecondarySMTPAddress = s.Secondary.Address
		c.SecondarySMTPTLS = s.Secondary.TLS
		c.SecondarySMTPUser = s.Secondary.Username
		var e error
		c.SecondarySMTPPassword, e = decrypt(s.Secondary.PasswordEnc)
		if e != nil {
			return c, e
		}
	}
	if s.PMG != nil {
		c.PMGURL = s.PMG.URL
		c.PMGUser = s.PMG.Username
		c.PMGNodes = s.PMG.Nodes
		c.PMGCAPEM = s.PMG.CAPEM
		c.PMGCAFile = ""
		var e error
		c.PMGPassword, e = decrypt(s.PMG.PasswordEnc)
		if e != nil {
			return c, e
		}
	}
	if s.OIDC != nil {
		c.Issuer = s.OIDC.Issuer
		c.ProviderName = s.OIDC.ProviderName
		c.ClientID = s.OIDC.ClientID
		c.OIDCDisabled = !s.OIDC.Enabled
		c.OIDCScopes = s.OIDC.Scopes
		c.GroupsClaim = s.OIDC.GroupsClaim
		var e error
		c.ClientSecret, e = decrypt(s.OIDC.ClientSecretEnc)
		if e != nil {
			return c, e
		}
	}
	if c.ProviderName == "" {
		c.ProviderName = "Single sign-on"
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if len(c.OIDCScopes) == 0 {
		c.OIDCScopes = []string{"openid", "profile", "email", "groups", "offline_access"}
	}
	return c, nil
}
func (a *App) saveRuntime(ctx context.Context, group string, c Config) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var data []byte
	if e = tx.QueryRow(ctx, "SELECT runtime_config FROM platform_settings WHERE id=1 FOR UPDATE").Scan(&data); e != nil {
		return e
	}
	var s RuntimeSettings
	if e = json.Unmarshal(data, &s); e != nil {
		return e
	}
	switch group {
	case "smtp":
		s.SMTP = &SMTPSettings{c.SMTPAddress, c.SMTPTLS, c.SMTPUser, seal(a.C.Secret, c.SMTPPassword), c.BounceDomain}
	case "secondary":
		s.Secondary = &SMTPSettings{Address: c.SecondarySMTPAddress, TLS: c.SecondarySMTPTLS, Username: c.SecondarySMTPUser, PasswordEnc: seal(a.C.Secret, c.SecondarySMTPPassword)}
	case "pmg":
		s.PMG = &PMGSettings{c.PMGURL, c.PMGUser, seal(a.C.Secret, c.PMGPassword), c.PMGNodes, c.PMGCAPEM}
	case "oidc":
		s.OIDC = &OIDCSettings{c.ssoEnabled(), c.Issuer, c.ProviderName, c.ClientID, seal(a.C.Secret, c.ClientSecret), c.OIDCScopes, c.GroupsClaim}
		s.PublicURL = c.PublicURL
	default:
		return fmt.Errorf("unknown configuration group")
	}
	data, e = json.Marshal(s)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE platform_settings SET runtime_config=$1 WHERE id=1", data); e != nil {
		return e
	}
	if group == "oidc" {
		if _, e = tx.Exec(ctx, "DELETE FROM sessions WHERE method='sso'"); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "DELETE FROM oauth_states"); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func validateSMTP(address, mode string, development, optional bool) error {
	if address == "" && optional {
		return nil
	}
	host, port, e := net.SplitHostPort(address)
	if e != nil || host == "" || port == "" || strings.ContainsAny(address, "\r\n") {
		return fmt.Errorf("SMTP address must be host:port")
	}
	if mode != "tls" && mode != "starttls" && !(development && mode == "none") {
		return fmt.Errorf("SMTP TLS mode must be STARTTLS or TLS; unencrypted is development-only")
	}
	return nil
}
func validateIssuer(value string, development bool) error {
	u, e := url.Parse(value)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Scheme != "https" && !(development && u.Scheme == "http") {
		return fmt.Errorf("use an HTTPS URL without embedded credentials or query parameters")
	}
	return nil
}
func testSMTP(ctx context.Context, c Config, secondary bool) error {
	address, mode, user, password := c.SMTPAddress, c.SMTPTLS, c.SMTPUser, c.SMTPPassword
	if secondary {
		address, mode, user, password = c.SecondarySMTPAddress, c.SecondarySMTPTLS, c.SecondarySMTPUser, c.SecondarySMTPPassword
	}
	if e := validateSMTP(address, mode, c.Dev, false); e != nil {
		return e
	}
	host, _, _ := net.SplitHostPort(address)
	dialer := net.Dialer{Timeout: 8 * time.Second}
	conn, e := dialer.DialContext(ctx, "tcp", address)
	if e != nil {
		return fmt.Errorf("SMTP connection failed; check host and port")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(12 * time.Second))
	if mode == "tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if e = tlsConn.HandshakeContext(ctx); e != nil {
			return fmt.Errorf("SMTP TLS certificate or handshake failed")
		}
		conn = tlsConn
	}
	client, e := smtp.NewClient(conn, host)
	if e != nil {
		return fmt.Errorf("SMTP greeting failed")
	}
	defer client.Close()
	if mode == "starttls" {
		if e = client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); e != nil {
			return fmt.Errorf("SMTP STARTTLS failed")
		}
	}
	if user != "" {
		if e = client.Auth(smtp.PlainAuth("", user, password, host)); e != nil {
			return fmt.Errorf("SMTP authentication failed")
		}
	}
	if e = client.Noop(); e != nil {
		return fmt.Errorf("SMTP connection test failed")
	}
	return nil
}
func (a *App) testPMG(ctx context.Context, c Config) (string, error) {
	copy := *a
	copy.C = c
	api, e := copy.pmgClient()
	if e != nil {
		return "", e
	}
	defer api.client.CloseIdleConnections()
	if e = api.login(ctx, c.PMGUser, c.PMGPassword); e != nil {
		return "", e
	}
	nodes := c.PMGNodes
	if len(nodes) == 0 {
		var rows []struct {
			Node string `json:"node"`
		}
		if e = api.request(ctx, "GET", "/nodes", nil, &rows); e != nil {
			return "", e
		}
		for _, r := range rows {
			nodes = append(nodes, r.Node)
		}
	}
	if len(nodes) == 0 {
		return "", fmt.Errorf("PMG returned no accessible nodes")
	}
	for _, node := range nodes {
		var records []pmgRecord
		if e = api.request(ctx, "GET", "/nodes/"+url.PathEscape(node)+"/tracker", url.Values{"starttime": {fmt.Sprint(time.Now().Add(-time.Minute).Unix())}, "endtime": {fmt.Sprint(time.Now().Unix())}, "limit": {"1"}}, &records); e != nil {
			return "", e
		}
	}
	return "Tracking access verified for: " + strings.Join(nodes, ", "), nil
}
func validateCAPEM(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 128<<10 {
		return fmt.Errorf("CA bundle exceeds 128 KiB")
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(value)) {
		return fmt.Errorf("CA bundle must contain PEM certificates")
	}
	return nil
}
