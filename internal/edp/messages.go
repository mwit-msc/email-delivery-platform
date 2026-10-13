package edp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/microcosm-cc/bluemonday"
	xhtml "golang.org/x/net/html"
	"html"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var placeholders = regexp.MustCompile(`\{\{\s*([a-zA-Z][a-zA-Z0-9_]*)\s*\}\}`)
var encodedPlaceholders = regexp.MustCompile(`(?i)%7b%7b(?:%20|\+)*([a-z][a-z0-9_]*)(?:%20|\+)*%7d%7d`)

func cleanHTML(s string) string {
	p := bluemonday.UGCPolicy()
	p.AllowElements("table", "thead", "tbody", "tfoot", "tr", "td", "th", "div", "span", "center")
	p.AllowAttrs("width", "height", "align", "valign", "cellpadding", "cellspacing", "border", "bgcolor").OnElements("table", "tr", "td", "th", "img", "div")
	p.AllowStyles("color", "background-color", "font-family", "font-size", "font-weight", "font-style", "line-height", "text-align", "text-decoration", "padding", "padding-top", "padding-bottom", "padding-left", "padding-right", "margin", "margin-top", "margin-bottom", "margin-left", "margin-right", "width", "height", "max-width", "border", "border-radius", "border-collapse", "vertical-align").Globally()
	// URL sanitization encodes braces in href/src. Retain template tokens after
	// sanitization; rendered values are escaped, URL-checked, and sanitized again.
	return encodedPlaceholders.ReplaceAllString(p.Sanitize(s), `{{$1}}`)
}
func plainHTML(s string) string {
	z := xhtml.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		t := z.Next()
		if t == xhtml.ErrorToken {
			break
		}
		if t == xhtml.TextToken {
			b.WriteString(string(z.Text()))
		}
		if t == xhtml.StartTagToken || t == xhtml.EndTagToken {
			tag, _ := z.TagName()
			switch string(tag) {
			case "br", "p", "div", "tr":
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}
func validAddress(s string) bool {
	a, e := mail.ParseAddress(s)
	return e == nil && a.Address == s && !strings.ContainsAny(s, "\r\n") && len(s) <= 254
}
func renderVariables(s string, vars map[string]string, escape bool) (string, error) {
	var missing string
	var listError error
	out := placeholders.ReplaceAllStringFunc(s, func(match string) string {
		key := placeholders.FindStringSubmatch(match)[1]
		if key == "certificate_list" && vars["certificate_items"] != "" {
			value, err := certificateList(vars["certificate_items"], vars["certificate_link_text"], escape)
			if err != nil {
				listError = err
			}
			return value
		}
		v, ok := vars[key]
		if !ok || strings.TrimSpace(v) == "" {
			missing = key
			return match
		}
		if strings.Contains(strings.ToLower(key), "url") {
			u, e := url.Parse(v)
			if e != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
				missing = key + " (expected HTTP(S) URL)"
				return match
			}
		}
		if escape {
			return html.EscapeString(v)
		}
		return v
	})
	if listError != nil {
		return "", listError
	}
	if missing != "" {
		return "", fmt.Errorf("missing or invalid variable: %s", missing)
	}
	return out, nil
}

type SendInput struct {
	ApplicationID string            `json:"application_id"`
	To            string            `json:"to"`
	CC            []string          `json:"cc,omitempty"`
	Template      string            `json:"template"`
	Version       int               `json:"template_version"`
	Variables     map[string]string `json:"variables"`
	Class         string            `json:"class"`
	ExpiresAt     *time.Time        `json:"expires_at"`
	ScheduleAt    *time.Time        `json:"schedule_at"`
}
type Rendered struct{ Subject, HTML, Plain string }

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (a *App) renderMessage(ctx context.Context, app, slug string, version int, vars map[string]string) (Rendered, error) {
	return renderMessageWith(ctx, a.DB, app, slug, version, vars)
}
func renderMessageWith(ctx context.Context, query rowQuerier, app, slug string, version int, vars map[string]string) (Rendered, error) {
	var r Rendered
	var required []string
	e := query.QueryRow(ctx, `SELECT v.subject,v.html,v.plain,v.variables FROM templates t JOIN template_versions v ON v.template_id=t.id AND v.version=CASE WHEN $3=0 THEN t.current_version ELSE $3 END WHERE t.application_id=$1 AND (t.slug=$2 OR t.id=$2)`, app, slug, version).Scan(&r.Subject, &r.HTML, &r.Plain, &required)
	if e != nil {
		return r, fmt.Errorf("template or version not found")
	}
	for _, k := range required {
		if k == "certificate_list" && vars["certificate_items"] != "" {
			k = "certificate_items"
		}
		if strings.TrimSpace(vars[k]) == "" {
			return r, fmt.Errorf("missing variable: %s", k)
		}
	}
	if r.Subject, e = renderVariables(r.Subject, vars, false); e != nil {
		return r, e
	}
	if len(r.Subject) > 998 {
		return r, fmt.Errorf("rendered subject exceeds 998 bytes")
	}
	if strings.ContainsAny(r.Subject, "\r\n") {
		return r, fmt.Errorf("subject contains a newline")
	}
	// Older saved versions may contain URL-encoded placeholder braces. Decode
	// through the same sanitizer before substitution, without rewriting versions.
	r.HTML = cleanHTML(r.HTML)
	if r.HTML, e = renderVariables(r.HTML, vars, true); e != nil {
		return r, e
	}
	r.HTML = cleanHTML(r.HTML)
	if r.Plain, e = renderVariables(r.Plain, vars, false); e != nil {
		return r, e
	}
	return r, nil
}

type HTTPError struct {
	Code    int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }
func (a *App) Submit(ctx context.Context, in SendInput, key string) (string, bool, error) {
	if len(key) < 1 || len(key) > 200 {
		return "", false, &HTTPError{400, "Idempotency-Key is required (maximum 200 characters)"}
	}
	if !validAddress(in.To) {
		return "", false, &HTTPError{400, "invalid recipient address"}
	}
	cc, err := normalizeCC(in.To, in.CC)
	if err != nil {
		return "", false, &HTTPError{400, err.Error()}
	}
	in.CC = cc
	if in.Class == "" {
		in.Class = "transactional"
	}
	if in.Class != "transactional" && in.Class != "otp" {
		return "", false, &HTTPError{400, "bulk messages must use an approved campaign"}
	}
	data, _ := json.Marshal(in)
	fingerprint := tokenHash(string(data))
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return "", false, e
	}
	defer tx.Rollback(ctx)
	var paused bool
	var from, reply, fromName string
	var daily, recipientLimit, queueLimit int
	e = tx.QueryRow(ctx, "SELECT paused,from_address,reply_to,from_name,daily_limit,recipient_hour_limit,queue_limit FROM applications WHERE id=$1 FOR UPDATE", in.ApplicationID).Scan(&paused, &from, &reply, &fromName, &daily, &recipientLimit, &queueLimit)
	if e != nil {
		return "", false, &HTTPError{404, "application not found"}
	}
	var oldID, oldHash string
	e = tx.QueryRow(ctx, "SELECT id,payload_hash FROM messages WHERE application_id=$1 AND idempotency_key=$2", in.ApplicationID, key).Scan(&oldID, &oldHash)
	if e == nil {
		if oldHash != fingerprint {
			return "", false, &HTTPError{409, "idempotency key was already used with a different payload"}
		}
		return oldID, true, nil
	}
	if e != pgx.ErrNoRows {
		return "", false, e
	}
	if paused {
		return "", false, &HTTPError{409, "application is paused"}
	}
	var globallyPaused bool
	if e = tx.QueryRow(ctx, "SELECT paused FROM platform_settings WHERE id=1").Scan(&globallyPaused); e != nil {
		return "", false, e
	}
	if globallyPaused {
		return "", false, &HTTPError{503, "sending is paused by the platform"}
	}
	var dayCount, queued int
	e = tx.QueryRow(ctx, `SELECT coalesce(sum(1+cardinality(cc)) FILTER(WHERE created_at>=now()-interval '24 hours'),0),count(*) FILTER(WHERE status IN('queued','retry_wait','sending')) FROM messages WHERE application_id=$1`, in.ApplicationID).Scan(&dayCount, &queued)
	if e != nil {
		return "", false, e
	}
	if dayCount+1+len(cc) > daily || queued >= queueLimit {
		return "", false, &HTTPError{429, "application quota, recipient cooldown, or queue capacity reached"}
	}
	additions := map[string]int{strings.ToLower(in.To): 1}
	for _, address := range cc {
		additions[strings.ToLower(address)]++
	}
	if err := checkRecipientCapacity(ctx, tx, in.ApplicationID, additions, recipientLimit); err != nil {
		return "", false, &HTTPError{429, err.Error()}
	}
	expires := time.Now().Add(24 * time.Hour)
	if in.Class == "otp" {
		expires = time.Now().Add(5 * time.Minute)
	}
	if in.ExpiresAt != nil {
		expires = *in.ExpiresAt
	}
	scheduled := time.Now()
	if in.ScheduleAt != nil {
		scheduled = *in.ScheduleAt
	}
	if in.Class == "otp" && expires.After(time.Now().Add(5*time.Minute)) {
		return "", false, &HTTPError{400, "OTP expiry cannot exceed five minutes"}
	}
	if expires.Before(scheduled) || expires.After(time.Now().Add(30*24*time.Hour)) {
		return "", false, &HTTPError{400, "expiry must follow scheduling and be within 30 days"}
	}
	rendered, e := renderMessageWith(ctx, tx, in.ApplicationID, in.Template, in.Version, in.Variables)
	if e != nil {
		return "", false, &HTTPError{400, e.Error()}
	}
	relayID, relayError := senderRelayWith(ctx, tx, from)
	if relayError != nil {
		return "", false, &HTTPError{400, relayError.Error()}
	}
	id := newID("msg_")
	_, e = tx.Exec(ctx, `INSERT INTO messages(id,application_id,recipient,from_address,reply_to,subject,html,plain,class,idempotency_key,payload_hash,available_at,expires_at,relay_id,cc,from_name) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),$15,$16)`, id, in.ApplicationID, in.To, from, reply, rendered.Subject, rendered.HTML, rendered.Plain, in.Class, key, fingerprint, scheduled, expires, relayID, cc, fromName)
	if e != nil {
		return "", false, e
	}
	_, e = tx.Exec(ctx, "INSERT INTO delivery_events(message_id,status) VALUES($1,'queued')", id)
	if e != nil {
		return "", false, e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", false, e
	}
	return id, false, nil
}

// Editor project data is untrusted too: it must not restore executable component
// scripts or event handlers when another authorized editor opens a draft.
func cleanProject(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if e := json.Unmarshal(raw, &value); e != nil {
		return nil, e
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case []any:
			out := []any{}
			for _, entry := range x {
				if clean := walk(entry); clean != nil {
					out = append(out, clean)
				}
			}
			return out
		case map[string]any:
			if tag, ok := x["tagName"].(string); ok && contains([]string{"script", "iframe", "object", "embed"}, strings.ToLower(tag)) {
				return nil
			}
			if kind, ok := x["type"].(string); ok && kind == "script" {
				return nil
			}
			out := map[string]any{}
			for key, data := range x {
				lower := strings.ToLower(key)
				if lower == "script" || lower == "script-export" || lower == "scripts" || lower == "srcdoc" || strings.HasPrefix(lower, "on") {
					continue
				}
				if lower == "content" {
					if text, ok := data.(string); ok {
						out[key] = cleanHTML(text)
						continue
					}
				}
				if lower == "href" || lower == "src" {
					if text, ok := data.(string); ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "javascript:") {
						continue
					}
				}
				out[key] = walk(data)
			}
			return out
		default:
			return v
		}
	}
	return json.Marshal(walk(value))
}
