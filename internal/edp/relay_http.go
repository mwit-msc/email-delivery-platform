package edp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func relayForm(r *http.Request) RelayInput {
	return RelayInput{ID: r.FormValue("relay_id"), Name: r.FormValue("name"), Address: r.FormValue("address"), Username: r.FormValue("username"), Password: r.FormValue("password"), TLS: r.FormValue("tls"), BounceDomain: r.FormValue("bounce_domain"), AllowedDomains: strings.FieldsFunc(r.FormValue("allowed_domains"), func(c rune) bool { return c == ',' || c == '\n' || c == ' ' }), Enabled: r.FormValue("enabled") == "on", FallbackID: r.FormValue("fallback_id"), ClearPassword: r.FormValue("password_clear") == "on"}
}
func (a *App) relaySettingsAction(w http.ResponseWriter, r *http.Request) bool {
	action := r.FormValue("action")
	if !strings.HasPrefix(action, "relay.") && action != "routing.save" && action != "domain.save" {
		return false
	}
	var e error
	switch action {
	case "relay.save":
		_, e = a.saveRelay(r.Context(), relayForm(r))
	case "relay.test":
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		e = a.testRelay(ctx, relayForm(r))
		if e != nil {
			problem(w, 400, e.Error())
		} else {
			respond(w, 200, map[string]any{"success": true, "message": "SMTP connection verified. No email was submitted."})
		}
		return true
	case "domain.save":
		e = a.setDomainRoute(r.Context(), r.FormValue("domain"), r.FormValue("relay_id"))
	case "routing.save":
		_, e = a.DB.Exec(r.Context(), "UPDATE platform_settings SET strict_sender_domains=$1 WHERE id=1", r.FormValue("strict") == "on")
	default:
		e = fmt.Errorf("unknown relay action")
	}
	if e != nil {
		problem(w, 400, e.Error())
		return true
	}
	a.audit(r.Context(), person(r).ID, "configuration."+action, "*", "")
	http.Redirect(w, r, "/settings?tab=smtp&saved=1", 303)
	return true
}
func (a *App) apiRelays(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.apiScope(w, r, "relay.manage"); !ok {
		return
	}
	if r.Method == "GET" {
		rows, e := a.listRelays(r.Context())
		if e != nil {
			problem(w, 500, "relays unavailable")
			return
		}
		routes, _ := a.query(r.Context(), "SELECT domain,relay_id FROM sender_domain_routes ORDER BY domain")
		settings, _ := a.one(r.Context(), "SELECT strict_sender_domains FROM platform_settings WHERE id=1")
		respond(w, 200, map[string]any{"relays": rows, "routes": routes, "strict": settings["strict_sender_domains"]})
		return
	}
	if op := r.PathValue("operation"); op != "" && op != "test" {
		problem(w, 400, "unknown relay operation")
		return
	}
	var in RelayInput
	if !decodeAPI(w, r, &in) {
		return
	}
	if r.PathValue("id") != "" {
		in.ID = r.PathValue("id")
	}
	if r.PathValue("operation") == "test" {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if e := a.testRelay(ctx, in); e != nil {
			problem(w, 400, e.Error())
			return
		}
		respond(w, 200, map[string]any{"success": true, "message": "Connection verified; no email submitted"})
		return
	}
	id, e := a.saveRelay(r.Context(), in)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "relay.save", "*", id)
	respond(w, 200, map[string]any{"id": id})
}
func (a *App) apiRouting(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.apiScope(w, r, "relay.manage"); !ok {
		return
	}
	var in struct {
		Strict  *bool  `json:"strict"`
		Domain  string `json:"domain"`
		RelayID string `json:"relay_id"`
	}
	if !decodeAPI(w, r, &in) {
		return
	}
	if in.Strict != nil && in.Domain != "" {
		problem(w, 400, "change a domain route or strict mode in one request")
		return
	}
	var e error
	if in.Strict != nil {
		_, e = a.DB.Exec(r.Context(), "UPDATE platform_settings SET strict_sender_domains=$1 WHERE id=1", *in.Strict)
	} else {
		e = a.setDomainRoute(r.Context(), in.Domain, in.RelayID)
	}
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "relay.routing", "*", "")
	respond(w, 200, map[string]any{"success": true})
}
