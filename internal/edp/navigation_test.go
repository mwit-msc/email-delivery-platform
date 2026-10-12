package edp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNavigationRequiresLiveSession(t *testing.T) {
	a := testApp(t)
	request := httptest.NewRequest("GET", "/settings", nil)
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Header().Get("HX-Redirect") != "/login" || response.Body.Len() != 0 {
		t.Fatalf("expired-session navigation: %d %s", response.Code, response.Body)
	}
	session := runtimeTestSession(t, a, true)
	for _, path := range []string{"/", "/settings?tab=oidc", "/templates/tpl_welcome", "/campaigns?app=app_demo"} {
		request = httptest.NewRequest("GET", path, nil)
		request.Header.Set("HX-Request", "true")
		request.Header.Set("HX-Boosted", "true")
		request.AddCookie(session)
		response = httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		body := response.Body.String()
		if response.Code != 200 || !strings.Contains(body, `id="page-content"`) || !strings.Contains(body, `id="edp-sidebar"`) || !strings.Contains(body, `hx-history="false"`) {
			t.Fatalf("navigation response %s: %d", path, response.Code)
		}
	}
}
