package edp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pmgRecord mirrors the MailTracker API, including its optional relay status.
type pmgRecord struct {
	ID      string   `json:"id"`
	MsgID   string   `json:"msgid"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	DStatus string   `json:"dstatus"`
	RStatus string   `json:"rstatus"`
	Time    int64    `json:"time"`
	Logs    []string `json:"logs"`
}

func normalizePMGMessageID(value string) string {
	value = strings.Trim(strings.TrimSpace(value), "<>")
	local, domain, found := strings.Cut(value, "@")
	if !found || local == "" || domain == "" {
		return ""
	}
	return local
}

func correlatePMGRecord(record pmgRecord, messageID, recipient, envelope string) bool {
	return record.ID != "" && normalizePMGMessageID(record.MsgID) == messageID &&
		strings.Trim(record.To, "<>") == recipient && strings.Trim(record.From, "<>") == envelope
}

// Security/filter rejections (B, Q, N) are deliberately not transport bounces.
func pmgStatus(record pmgRecord) string {
	status := record.DStatus
	if status == "A" && record.RStatus != "" {
		status = record.RStatus
	}
	switch status {
	case "2":
		return "remote_accepted"
	case "4":
		return "remote_deferred"
	case "5":
		return "bounced"
	case "A":
		return "relay_accepted"
	default:
		return ""
	}
}

var pmgLogRecipient = regexp.MustCompile(`\bto=<([^>]+)>`)
var pmgPermanentReply = regexp.MustCompile(`\bdsn=5\.[0-9]+\.[0-9]+\b|\b5[0-9]{2}[ -]`)
var pmgBouncedTransport = regexp.MustCompile(`\bpostfix/(?:smtp|lmtp)\[[0-9]+\]:`)
var pmgBouncedStatus = regexp.MustCompile(`\bstatus=bounced\b`)

// Only a permanent transport response for this recipient can authorize a
// reputation-based fallback. Queue-wide filter/security logs never qualify.
func pmgBounceDetail(record pmgRecord, recipient string) (string, string) {
	for i := len(record.Logs) - 1; i >= 0; i-- {
		line := record.Logs[i]
		to := pmgLogRecipient.FindStringSubmatch(line)
		if len(to) != 2 || to[1] != recipient || !pmgBouncedTransport.MatchString(line) ||
			!pmgBouncedStatus.MatchString(line) || !pmgPermanentReply.MatchString(line) {
			continue
		}
		category := classifyFailure(line)
		return pmgDetail(line), category
	}
	detail := "PMG reports a permanent transport bounce; no correlated permanent recipient response was available"
	return detail, classifyFailure(detail)
}

func pmgDetail(detail string) string {
	detail = strings.ReplaceAll(strings.ReplaceAll(detail, "\r", " "), "\n", " ")
	runes := []rune(detail)
	if len(runes) > 512 {
		detail = string(runes[:512])
	}
	return detail
}

type pmgAPI struct {
	base   string
	client *http.Client
	ticket string
}

func (a *App) pmgClient() (*pmgAPI, error) {
	base, err := url.Parse(a.C.PMGURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		(base.Scheme != "https" && !(a.C.Dev && base.Scheme == "http")) {
		return nil, fmt.Errorf("PMG_URL must be an HTTPS gateway URL without credentials, query, or fragment")
	}
	if a.C.PMGUser == "" || a.C.PMGPassword == "" {
		return nil, fmt.Errorf("PMG polling requires PMG_USERNAME and PMG_PASSWORD for a read-only audit account")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if a.C.PMGCAFile != "" || a.C.PMGCAPEM != "" {
		pem := []byte(a.C.PMGCAPEM)
		if len(pem) == 0 {
			var err error
			pem, err = os.ReadFile(a.C.PMGCAFile)
			if err != nil {
				return nil, fmt.Errorf("cannot read PMG_CA_FILE")
			}
		}

		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("PMG_CA_FILE contains no PEM certificates")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	base.Path = strings.TrimSuffix(strings.TrimRight(base.Path, "/"), "/api2/json") + "/api2/json"
	return &pmgAPI{base: strings.TrimRight(base.String(), "/"), client: &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("PMG redirects are unsupported; configure the final gateway URL")
		},
	}}, nil
}

func (api *pmgAPI) request(ctx context.Context, method, path string, values url.Values, target any) error {
	endpoint := api.base + path
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(values.Encode())
	} else if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("cannot construct PMG API request")
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if api.ticket != "" {
		req.AddCookie(&http.Cookie{Name: "PMGAuthCookie", Value: api.ticket})
	}
	response, err := api.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("PMG request failed; verify connectivity and certificate trust")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("PMG API returned HTTP %d; verify endpoint, audit permissions, and authentication", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return fmt.Errorf("PMG API response exceeds the 2 MiB limit or could not be read")
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("PMG API returned invalid or missing data")
	}
	if json.Unmarshal(envelope.Data, target) != nil {
		return fmt.Errorf("PMG API returned an unexpected response format")
	}
	return nil
}

func (api *pmgAPI) login(ctx context.Context, username, password string) error {
	var result struct {
		Ticket string `json:"ticket"`
	}
	if err := api.request(ctx, http.MethodPost, "/access/ticket", url.Values{"username": {username}, "password": {password}}, &result); err != nil {
		return err
	}
	if result.Ticket == "" || strings.Contains(result.Ticket, "!tfa!") {
		return fmt.Errorf("PMG authentication requires a non-interactive audit account")
	}
	api.ticket = result.Ticket
	return nil
}

// PollPMG reads gateway evidence and reconciles at most twenty primary-route
// messages. SMTP dispatch runs independently; polling never submits email.
func (a *App) PollPMG(ctx context.Context) error {
	if a.C.PMGURL == "" {
		return nil
	}
	api, err := a.pmgClient()
	if err != nil {
		return err
	}
	defer api.client.CloseIdleConnections()
	if err = api.login(ctx, a.C.PMGUser, a.C.PMGPassword); err != nil {
		return err
	}
	nodes := append([]string(nil), a.C.PMGNodes...)
	if len(nodes) == 0 {
		var discovered []struct {
			Node string `json:"node"`
		}
		if err = api.request(ctx, http.MethodGet, "/nodes", nil, &discovered); err != nil {
			return err
		}
		for _, node := range discovered {
			if node.Node != "" {
				nodes = append(nodes, node.Node)
			}
		}
	}
	if len(nodes) == 0 || len(nodes) > 20 {
		return fmt.Errorf("PMG polling requires between one and twenty gateway nodes")
	}
	type candidate struct {
		ID, Recipient, From string
		Envelope            *string
		Created             time.Time
	}
	rows, err := a.DB.Query(ctx, `SELECT id,recipient,from_address,created_at,envelope_sender FROM messages WHERE route='primary' AND status IN('relay_accepted','unknown') AND created_at>=now()-interval '7 days' ORDER BY pmg_checked_at NULLS FIRST,updated_at,id LIMIT 20`)
	if err != nil {
		return fmt.Errorf("cannot load messages for PMG polling: %w", err)
	}
	var candidates []candidate
	for rows.Next() {
		var message candidate
		if err = rows.Scan(&message.ID, &message.Recipient, &message.From, &message.Created, &message.Envelope); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, message)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, message := range candidates {
		envelope := message.From
		if message.Envelope != nil {
			envelope = *message.Envelope
		} else if a.C.BounceDomain != "" {
			envelope = "bounce+" + message.ID + "@" + a.C.BounceDomain
		}
		start, end := message.Created.Add(-time.Minute).Unix(), time.Now().Unix()
		window := url.Values{"starttime": {strconv.FormatInt(start, 10)}, "endtime": {strconv.FormatInt(end, 10)}}
		type evidence struct {
			Node   string
			Record pmgRecord
		}
		var matches []evidence
		for _, node := range nodes {
			path := "/nodes/" + url.PathEscape(node) + "/tracker"
			query := url.Values{"starttime": window["starttime"], "endtime": window["endtime"], "target": {message.Recipient}, "from": {envelope}, "limit": {"2000"}}
			var records []pmgRecord
			if err = api.request(ctx, http.MethodGet, path, query, &records); err != nil {
				return err
			}
			for _, record := range records {
				if !correlatePMGRecord(record, message.ID, message.Recipient, envelope) || record.Time < start || record.Time > end {
					continue
				}
				if pmgStatus(record) != "" {
					matches = append(matches, evidence{node, record})
				}
			}
		}
		// A successful copy rules out automatic resubmission even if another
		// node records an older bounce. Otherwise reconcile only latest evidence.
		sort.SliceStable(matches, func(i, j int) bool {
			left, right := pmgStatus(matches[i].Record), pmgStatus(matches[j].Record)
			if (left == "remote_accepted") != (right == "remote_accepted") {
				return left == "remote_accepted"
			}
			if matches[i].Record.Time != matches[j].Record.Time {
				return matches[i].Record.Time > matches[j].Record.Time
			}
			rank := map[string]int{"remote_accepted": 4, "bounced": 3, "remote_deferred": 2, "relay_accepted": 1}
			return rank[left] > rank[right]
		})
		if len(matches) > 0 {
			match := matches[0]
			record, node := match.Record, match.Node
			status := pmgStatus(record)
			detail, category := "PMG tracker reports "+status, ""
			if status == "bounced" {
				var detailed pmgRecord
				path := "/nodes/" + url.PathEscape(node) + "/tracker/" + url.PathEscape(record.ID)
				if err = api.request(ctx, http.MethodGet, path, window, &detailed); err != nil {
					return err
				}
				if !correlatePMGRecord(detailed, message.ID, message.Recipient, envelope) || pmgStatus(detailed) != "bounced" {
					return fmt.Errorf("PMG detailed bounce evidence did not match the original message")
				}
				detail, category = pmgBounceDetail(detailed, message.Recipient)
			}
			key := "pmg:" + node + ":" + record.ID + ":" + message.ID + ":" + record.DStatus + ":" + record.RStatus
			if err = a.applyGatewayOutcome(ctx, message.ID, key, status, detail, category); err != nil {
				return err
			}
		}
		if _, err = a.DB.Exec(ctx, "UPDATE messages SET pmg_checked_at=now() WHERE id=$1", message.ID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) pmgLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		config, err := a.runtimeConfig(pollCtx)
		if err == nil && config.PMGURL != "" {
			copy := *a
			copy.C = config
			err = copy.PollPMG(pollCtx)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("PMG delivery polling failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
