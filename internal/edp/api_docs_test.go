package edp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apidocs "email-delivery-platform/api"
)

func TestAPIDocumentationRequiresEDPAuthentication(t *testing.T) {
	a := testApp(t)
	paths := []string{"/docs", "/openapi.yaml", "/llms.txt", "/llms-full.txt"}
	for _, path := range paths {
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" || strings.Contains(w.Body.String(), "openapi:") {
			t.Fatal("documentation exposed without login", path, w.Code)
		}
	}
	cookie := runtimeTestSession(t, a, true)
	key := automationKey(t, a, "app_demo", []string{"template.view"}, nil)
	for _, path := range paths {
		for _, bearer := range []bool{false, true} {
			r := httptest.NewRequest("GET", path, nil)
			if bearer {
				r.Header.Set("Authorization", "Bearer "+key)
			} else {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(w.Header().Get("Vary"), "Authorization") {
				t.Fatal("authenticated documentation unavailable or cached", path, w.Code)
			}
			if path == "/openapi.yaml" && !bytes.Equal(w.Body.Bytes(), apidocs.OpenAPI) {
				t.Fatal("served schema differs from the repository contract")
			}
			if path == "/docs" && (!strings.Contains(w.Body.String(), "scalar-1.74.0.js") || strings.Contains(w.Body.String(), "cdn.jsdelivr.net")) {
				t.Fatal("Scalar is not self-hosted")
			}
		}
	}
	if _, err := a.DB.Exec(context.Background(), "UPDATE api_keys SET revoked=true WHERE token_hash=$1", tokenHash(key)); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("revoked key could retrieve docs", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/docs", nil)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || w.Header().Get("HX-Redirect") != "/login" {
		t.Fatal("expired session docs navigation did not redirect to login")
	}
}
