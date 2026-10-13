package edp

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
)

func validDisplayName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 120 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type BrandingCheck struct{ Name, Status, Detail, Record string }

// Inspect DNS only. Never fetch a logo/certificate URL supplied by DNS.
func checkInboxBranding(ctx context.Context, from string, lookup func(context.Context, string) ([]string, error)) []BrandingCheck {
	domain, err := senderDomain(from)
	if err != nil {
		return []BrandingCheck{{Name: "Sender domain", Status: "Invalid", Detail: "A valid sender domain is required."}}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	read := func(host, version string) (map[string]string, string, string) {
		records, err := lookup(ctx, host)
		if err != nil {
			var dns *net.DNSError
			if errors.As(err, &dns) && dns.IsNotFound {
				return nil, "", "No matching policy record was found."
			}
			return nil, "", "DNS lookup failed or no record was found. Check DNS and try again."
		}
		var found map[string]string
		var record string
		for _, value := range records {
			if !strings.HasPrefix(strings.TrimSpace(value), "v="+version) {
				continue
			}
			if found != nil {
				return nil, "", "Multiple records were found; publish one policy record."
			}
			found = map[string]string{}
			for _, part := range strings.Split(value, ";") {
				key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
				if !ok {
					continue
				}
				if _, duplicate := found[key]; duplicate {
					return nil, "", "Duplicate policy tags were found."
				}
				found[key] = strings.TrimSpace(val)
			}
			if found["v"] != version {
				return nil, "", "Invalid policy version."
			}
			record = value
		}
		if found == nil {
			return nil, "", "No matching policy record was found."
		}
		return found, record, ""
	}
	dmarc, raw, detail := read("_dmarc."+domain, "DMARC1")
	inherited := false
	if dmarc == nil && detail == "No matching policy record was found." {
		if org, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil && org != domain {
			dmarc, raw, detail = read("_dmarc."+org, "DMARC1")
			inherited = true
		}
	}
	policy := dmarc["p"]
	if inherited && dmarc["sp"] != "" {
		policy = dmarc["sp"]
	}
	dm := BrandingCheck{Name: "DMARC", Status: "Needs attention", Detail: detail, Record: raw}
	if dmarc != nil {
		if (policy == "quarantine" || policy == "reject") && (dmarc["pct"] == "" || dmarc["pct"] == "100") {
			dm.Status, dm.Detail = "Enforced policy published", "An enforced DNS policy was found. SPF/DKIM alignment of actual mail still needs verification."
		} else {
			dm.Detail = "BIMI requires quarantine or reject for all mail (pct=100)."
		}
	}
	bimi, raw, detail := read("default._bimi."+domain, "BIMI1")
	bi := BrandingCheck{Name: "BIMI", Status: "Not ready", Detail: detail, Record: raw}
	https := func(value string) bool {
		u, err := url.Parse(value)
		return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
	}
	if bimi != nil {
		if !https(bimi["l"]) {
			bi.Detail = "Publish an HTTPS logo URL in the l tag."
		} else if !https(bimi["a"]) {
			bi.Detail = "A logo is published; providers such as Gmail also require an HTTPS VMC/CMC certificate URL in the a tag."
		} else {
			bi.Status, bi.Detail = "URLs published", "Logo and certificate URLs are present. Their contents, certificate validity and mailbox eligibility have not been verified."
		}
	}
	return []BrandingCheck{dm, bi}
}
