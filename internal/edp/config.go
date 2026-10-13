package edp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	OIDCDisabled          bool
	OIDCScopes            []string
	GroupsClaim           string
	PMGCAPEM              string
	DatabaseURL           string
	Address               string
	PublicURL             string
	Issuer                string
	ClientID              string
	ClientSecret          string
	RootUsername          string
	RootPassword          string
	RootHash              string
	SMTPAddress           string
	SMTPUser              string
	SMTPPassword          string
	SMTPTLS               string
	BounceDomain          string
	GoogleCredentials     string
	MetricsToken          string
	ProviderName          string
	SeedCertificatesFile  string
	SeedLogoFile          string
	PMGURL                string
	PMGUser               string
	PMGPassword           string
	PMGCAFile             string
	SecondarySMTPAddress  string
	SecondarySMTPUser     string
	SecondarySMTPPassword string
	SecondarySMTPTLS      string
	PMGNodes              []string
	Secret                []byte
	Dev                   bool
	RetentionDays         int
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func LoadConfig() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"),
		OIDCScopes:            strings.Fields(env("OIDC_SCOPES", "openid profile email groups offline_access")),
		GroupsClaim:           env("OIDC_GROUPS_CLAIM", "groups"),
		Address:               env("LISTEN_ADDR", ":8080"),
		PublicURL:             strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		Issuer:                os.Getenv("OIDC_ISSUER"),
		ClientID:              os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret:          os.Getenv("OIDC_CLIENT_SECRET"),
		RootUsername:          env("ROOT_USERNAME", "root"),
		RootPassword:          os.Getenv("ROOT_PASSWORD"),
		RootHash:              os.Getenv("ROOT_PASSWORD_HASH"),
		SMTPAddress:           os.Getenv("SMTP_ADDR"),
		SMTPUser:              os.Getenv("SMTP_USERNAME"),
		SMTPPassword:          os.Getenv("SMTP_PASSWORD"),
		SMTPTLS:               env("SMTP_TLS", "starttls"),
		BounceDomain:          os.Getenv("BOUNCE_DOMAIN"),
		GoogleCredentials:     os.Getenv("GOOGLE_CREDENTIALS_FILE"),
		MetricsToken:          os.Getenv("METRICS_TOKEN"),
		ProviderName:          env("OIDC_PROVIDER_NAME", "Single sign-on"),
		SeedCertificatesFile:  os.Getenv("SEED_CERTIFICATES_FILE"),
		SeedLogoFile:          os.Getenv("SEED_LOGO_FILE"),
		PMGURL:                strings.TrimRight(os.Getenv("PMG_URL"), "/"),
		PMGUser:               os.Getenv("PMG_USERNAME"),
		PMGPassword:           os.Getenv("PMG_PASSWORD"),
		PMGCAFile:             os.Getenv("PMG_CA_FILE"),
		PMGNodes:              strings.FieldsFunc(os.Getenv("PMG_NODES"), func(r rune) bool { return r == ',' }),
		SecondarySMTPAddress:  os.Getenv("SECONDARY_SMTP_ADDR"),
		SecondarySMTPUser:     os.Getenv("SECONDARY_SMTP_USERNAME"),
		SecondarySMTPPassword: os.Getenv("SECONDARY_SMTP_PASSWORD"),
		SecondarySMTPTLS:      env("SECONDARY_SMTP_TLS", "starttls"),
		Dev:                   os.Getenv("EDP_ENV") == "development",
		RetentionDays:         30}
	if n, err := strconv.Atoi(env("RETENTION_DAYS", "30")); err == nil && n > 0 {
		c.RetentionDays = n
	}
	if c.Dev && c.SMTPAddress == "" {
		c.SMTPAddress = "mailpit:1025"
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	secret, err := base64.StdEncoding.DecodeString(os.Getenv("SECRET_KEY"))
	if err != nil || len(secret) != 32 {
		return c, errors.New("SECRET_KEY must be a base64-encoded 32-byte key")
	}
	c.Secret = secret
	if !c.Dev && !strings.HasPrefix(c.PublicURL, "https://") {
		return c, errors.New("PUBLIC_URL must use HTTPS outside development")
	}
	if c.SMTPTLS != "starttls" && c.SMTPTLS != "tls" && !(c.Dev && c.SMTPTLS == "none") {
		return c, errors.New("SMTP_TLS must be starttls or tls (none is development-only)")
	}
	if c.ClientID != "" && c.Issuer == "" {
		return c, errors.New("OIDC_ISSUER is required when SSO is enabled")
	}
	if c.PMGURL != "" {
		if !c.Dev && !strings.HasPrefix(c.PMGURL, "https://") {
			return c, errors.New("PMG_URL requires HTTPS")
		}
		if c.PMGUser == "" || c.PMGPassword == "" || len(c.PMGNodes) == 0 {
			return c, errors.New("PMG_USERNAME, PMG_PASSWORD, and PMG_NODES are required")
		}
	}
	if c.SecondarySMTPAddress != "" && c.SecondarySMTPTLS != "starttls" && c.SecondarySMTPTLS != "tls" && !(c.Dev && c.SecondarySMTPTLS == "none") {
		return c, errors.New("SECONDARY_SMTP_TLS must be starttls or tls (none is development-only)")
	}
	return c, nil
}
func newID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func tokenHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func seal(key []byte, s string) string {
	if s == "" {
		return ""
	}
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	n := make([]byte, g.NonceSize())
	if _, err := rand.Read(n); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(g.Seal(n, n, []byte(s), nil))
}
func unseal(key []byte, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	data, e := base64.RawURLEncoding.DecodeString(s)
	if e != nil {
		return "", e
	}
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	if len(data) < g.NonceSize() {
		return "", fmt.Errorf("invalid ciphertext")
	}
	p, e := g.Open(nil, data[:g.NonceSize()], data[g.NonceSize():], nil)
	return string(p), e
}

const sessionLife = 12 * time.Hour

func (c Config) ssoEnabled() bool { return c.ClientID != "" && !c.OIDCDisabled }
