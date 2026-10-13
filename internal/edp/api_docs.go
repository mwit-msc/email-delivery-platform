package edp

import (
	"net/http"

	apidocs "email-delivery-platform/api"
)

func (a *App) docs(w http.ResponseWriter, r *http.Request) {
	principal := person(r)
	if r.Header.Get("Authorization") != "" {
		principal = a.keyPrincipal(r)
		if principal == nil {
			problem(w, http.StatusUnauthorized, "valid service token required")
			return
		}
	}
	if principal == nil {
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusNoContent)
		} else {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie, Authorization")
	switch r.URL.Path {
	case "/openapi.yaml":
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Write(apidocs.OpenAPI)
	case "/llms-full.txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(apidocs.Guide)
	case "/llms.txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("# Email Delivery Platform\n\nAuthenticated API documentation. Send an EDP session cookie or Authorization: Bearer <service-token>.\n\n- [OpenAPI contract](/openapi.yaml): authoritative endpoint schemas and scopes.\n- [API integration guide](/llms-full.txt): templates, draft campaigns, delivery, and constraints.\n- [Interactive reference](/docs): Scalar API reference behind the same authentication gate.\n"))
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(apiDocsHTML))
	}
}

const apiDocsHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>EDP API reference</title><link rel="stylesheet" href="/assets/api-docs.css"><script src="/assets/scalar-1.74.0.js" defer></script><script src="/assets/api-docs.js" defer></script></head><body><header class="docs-bar"><a href="/">EDP dashboard</a><nav aria-label="Documentation formats"><a href="/openapi.yaml">OpenAPI YAML</a><a href="/llms.txt">LLM index</a><a href="/llms-full.txt">Integration guide</a></nav></header><main id="api-reference"></main><noscript>JavaScript is required for the interactive reference. Use the OpenAPI YAML or integration guide above.</noscript></body></html>`
