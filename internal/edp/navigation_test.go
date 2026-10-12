package edp

import (
	"context"
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

func TestCampaignProgressDoesNotReplaceNavigationContent(t *testing.T) {
	a := testApp(t)
	var user string
	if err := a.DB.QueryRow(context.Background(), "SELECT id FROM users WHERE root").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(context.Background(), "INSERT INTO campaigns(id,application_id,name,created_by) VALUES('cmp_navigation','app_demo','Synthetic navigation',$1)", user); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/campaigns/cmp_navigation", nil)
	request.AddCookie(runtimeTestSession(t, a, true))
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	start := strings.Index(body, `hx-get="/campaigns/cmp_navigation/progress"`)
	if response.Code != 200 || start < 0 {
		t.Fatal("campaign progress control missing")
	}
	control := strings.SplitN(body[start:], ">", 2)[0]
	for _, attribute := range []string{`hx-target="this"`, `hx-select="unset"`, `hx-select-oob="unset"`, `hx-push-url="false"`} {
		if !strings.Contains(control, attribute) {
			t.Fatalf("progress inherits navigation behavior: missing %s", attribute)
		}
	}
	request = httptest.NewRequest("GET", "/campaigns/cmp_navigation/progress", nil)
	request.AddCookie(runtimeTestSession(t, a, true))
	response = httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "draft") || strings.Contains(response.Body.String(), `id="page-content"`) {
		t.Fatal("progress must return a widget fragment")
	}
}
