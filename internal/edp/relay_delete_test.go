package edp

import (
	"context"
	"net/url"
	"sync"
	"testing"
)

func TestRelayDeletionReferencesAndPermissions(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	var root string
	a.DB.QueryRow(ctx, "SELECT id FROM users WHERE root").Scan(&root)
	key := automationKey(t, a, "app_demo", []string{"relay.manage"}, root)
	limited := automationKey(t, a, "app_demo", []string{"message.view"}, root)
	in := RelayInput{Name: "Disposable relay", Address: "localhost:1025", TLS: "none", AllowedDomains: []string{"example.test"}, Enabled: true}
	id, err := a.saveRelay(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/relays/" + id
	if code := automationRequest(t, a, limited, "DELETE", path, nil).Code; code != 403 {
		t.Fatal("relay delete ignored scope", code)
	}
	session := runtimeTestSession(t, a, true)
	if code := runtimeSettingsRequest(a, session, url.Values{"action": {"relay.delete"}, "relay_id": {id}}, false).Code; code != 403 {
		t.Fatal("relay delete ignored CSRF", code)
	}
	if err := a.setDomainRoute(ctx, "example.test", id); err != nil {
		t.Fatal(err)
	}
	if code := automationRequest(t, a, key, "DELETE", path, nil).Code; code != 409 {
		t.Fatal("deleted routed relay", code)
	}
	a.setDomainRoute(ctx, "example.test", "")
	in.FallbackID = id
	parent, err := a.saveRelay(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if code := automationRequest(t, a, key, "DELETE", path, nil).Code; code != 409 {
		t.Fatal("deleted fallback relay", code)
	}
	if err := a.deleteRelay(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if code := automationRequest(t, a, key, "DELETE", path, nil).Code; code != 204 {
		t.Fatal("unused relay not deleted", code)
	}
	if code := automationRequest(t, a, key, "DELETE", path, nil).Code; code != 404 {
		t.Fatal("missing relay not reported", code)
	}
	in.ID, in.FallbackID = "", ""
	id, err = a.saveRelay(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	a.setDomainRoute(ctx, "example.test", id)
	mid, _, err := a.Submit(ctx, SendInput{ApplicationID: "app_demo", To: "recipient@example.test", Template: "welcome", Variables: map[string]string{"name": "Synthetic"}}, "relay-history")
	if err != nil {
		t.Fatal(err)
	}
	a.setDomainRoute(ctx, "example.test", "")
	a.DB.Exec(ctx, "UPDATE messages SET status='remote_accepted' WHERE id=$1", mid)
	if err := a.deleteRelay(ctx, id); err == nil {
		t.Fatal("deleted delivery history relay")
	}
}

func TestRelayDeleteConcurrentRouting(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		id, err := a.saveRelay(ctx, RelayInput{Name: "Concurrent relay", Address: "localhost:1025", TLS: "none", AllowedDomains: []string{"example.test"}, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var routeErr, deleteErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; routeErr = a.setDomainRoute(ctx, "example.test", id) }()
		go func() { defer wg.Done(); <-start; deleteErr = a.deleteRelay(ctx, id) }()
		close(start)
		wg.Wait()
		if (routeErr == nil) == (deleteErr == nil) {
			t.Fatal("routing and deletion did not serialize", routeErr, deleteErr)
		}
		if routeErr == nil {
			a.setDomainRoute(ctx, "example.test", "")
			if err := a.deleteRelay(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
}
