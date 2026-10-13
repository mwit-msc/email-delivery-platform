package edp

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Certificate lists are structured data, never caller-supplied trusted HTML.
type CertificateItem struct {
	Name      string `json:"name"`
	Award     string `json:"award"`
	URL       string `json:"url"`
	SourceRow int    `json:"source_row,omitempty"`
}

func hasCertificateList(content string) bool {
	for _, match := range placeholders.FindAllStringSubmatch(content, -1) {
		if match[1] == "certificate_list" {
			return true
		}
	}
	return false
}

func certificateList(raw, linkText string, asHTML bool) (string, error) {
	if len(raw) > 2<<20 {
		return "", fmt.Errorf("certificate_items exceeds 2 MiB")
	}
	var items []CertificateItem
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&items); err != nil {
		return "", fmt.Errorf("certificate_items must be a JSON array of certificates")
	}
	if dec.Decode(new(any)) != io.EOF || len(items) == 0 || len(items) > 5000 {
		return "", fmt.Errorf("certificate_items requires 1–5000 certificates")
	}
	var out strings.Builder
	if strings.TrimSpace(linkText) == "" {
		linkText = "Open certificate"
	}
	if len(linkText) > 200 {
		return "", fmt.Errorf("certificate_link_text exceeds 200 bytes")
	}
	if asHTML {
		out.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="width:100%;border-collapse:collapse">`)
	}
	for i, item := range items {
		u, err := url.Parse(item.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(item.URL, "\r\n") || item.SourceRow < 0 || strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.Award) == "" || len(item.Name) > 4096 || len(item.Award) > 4096 || len(item.URL) > 4096 {
			return "", fmt.Errorf("invalid certificate item %d: name, award and HTTPS URL are required", i+1)
		}
		if asHTML {
			out.WriteString(`<tr><td style="padding:16px 0;border-bottom:1px solid #e2e8f0"><strong>` + html.EscapeString(item.Name) + `</strong><br><span style="color:#475569">` + html.EscapeString(item.Award) + `</span><br><a href="` + html.EscapeString(item.URL) + `" style="color:#1e40af;text-decoration:underline">` + html.EscapeString(linkText) + `</a></td></tr>`)
		} else {
			out.WriteString(strconv.Itoa(i+1) + ". " + item.Name + "\n" + item.Award + "\n" + item.URL + "\n\n")
		}
		if out.Len() > 2<<20 {
			return "", fmt.Errorf("rendered certificate list exceeds 2 MiB")
		}
	}
	if asHTML {
		out.WriteString(`</table>`)
	}
	return out.String(), nil
}

type mappedCertificateRow struct {
	Ordinal   int
	Recipient string
	Template  *string
	Version   *int
	Variables map[string]string
	Errors    []string
	Dispatch  string
	Sources   []int
	Excluded  bool
	GroupKey  string
}

func combineCertificateRows(rows []mappedCertificateRow) ([]mappedCertificateRow, error) {
	var out []mappedCertificateRow
	groups := map[string]int{}
	items := map[int][]CertificateItem{}
	for _, row := range rows {
		// Invalid and previously excluded certificates stay individually reviewable.
		if row.Excluded || len(row.Errors) > 0 {
			out = append(out, row)
			continue
		}
		var incoming []CertificateItem
		if err := json.Unmarshal([]byte(row.Variables["certificate_items"]), &incoming); err != nil {
			return nil, err
		}
		if index, ok := groups[row.GroupKey]; ok {
			group := &out[index]
			items[index] = append(items[index], incoming...)
			group.Sources = append(group.Sources, row.Sources...)
		} else {
			groups[row.GroupKey] = len(out)
			items[len(out)] = incoming
			out = append(out, row)
		}
	}
	for index, certificates := range items {
		data, _ := json.Marshal(certificates)
		row := &out[index]
		row.Variables["certificate_items"] = string(data)
		row.Variables["certificate_count"] = strconv.Itoa(len(certificates))
		row.Dispatch = tokenHash("certificate-group:" + row.GroupKey + ":" + string(data))
	}
	return out, nil
}

// Preserve local-part case: SMTP technically permits case-sensitive mailboxes.
func certificateRecipientKey(email string) string {
	if at := strings.LastIndexByte(email, '@'); at >= 0 {
		return email[:at] + "@" + strings.ToLower(email[at+1:])
	}
	return email
}
