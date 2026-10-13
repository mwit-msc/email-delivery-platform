package edp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func releasedSchemaApp(t *testing.T) *App {
	t.Helper()
	raw := os.Getenv("EDP_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("EDP_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := newID("edp_upgrade_")
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	var app *App
	t.Cleanup(func() {
		if app != nil {
			app.Close()
		}
		pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		pool.Close()
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	app, err = New(ctx, Config{DatabaseURL: u.String(), Dev: true, PublicURL: "http://localhost:8080", RootUsername: "root", RootPassword: "synthetic-upgrade-root-password", Secret: make([]byte, 32), SMTPAddress: "bootstrap.example.test:1025", SMTPTLS: "none", RetentionDays: 30}, "upgrade-test")
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the shipped v1 schema rather than downgrading a fresh install.
	tx, err := app.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, filename := range []string{"001_initial.sql", "002_dispatch_fairness.sql", "003_gateway_routes.sql", "004_generic_campaigns.sql", "005_runtime_configuration.sql", "006_envelope_sender.sql"} {
		data, err := resources.ReadFile("migrations/" + filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(data)); err != nil {
			t.Fatalf("released migration %s failed: %v", filename, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = app.Seed(ctx, true); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestReleasedV1UpgradePreservesDataAndDelivery(t *testing.T) {
	a := releasedSchemaApp(t)
	ctx := context.Background()
	var root, rootHash string
	if err := a.DB.QueryRow(ctx, "SELECT id,password_hash FROM users WHERE root").Scan(&root, &rootHash); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, `INSERT INTO template_versions(template_id,version,subject,html,plain,variables)
		VALUES('tpl_welcome',2,'Released version {{name}}','<p>Released {{name}}</p>','Released {{name}}',ARRAY['name']);
		UPDATE templates SET current_version=2 WHERE id='tpl_welcome'`); err != nil {
		t.Fatal(err)
	}
	// Keep a frozen old campaign and row, including its original dispatch identity.
	if _, err := a.DB.Exec(ctx, `INSERT INTO campaigns(id,application_id,name,state,kind,source_rows,created_by,approved_by,approved_at)
		VALUES('cmp_released','app_demo','Released campaign','approved','bulk','[["email","name"],["campaign@example.test","Synthetic"]]',$1,$1,now())`, root); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, `INSERT INTO campaign_rows(id,campaign_id,ordinal,recipient,template_id,template_version,variables,dispatch_key)
		VALUES('row_released','cmp_released',1,'campaign@example.test','tpl_welcome',1,'{"name":"Synthetic"}','released-dispatch')`); err != nil {
		t.Fatal(err)
	}
	legacyToken := newID("synthetic_upgrade_key_")
	if _, err := a.DB.Exec(ctx, "INSERT INTO api_keys(id,application_id,name,token_hash,scopes) VALUES('key_released','app_demo','Released service key',$1,ARRAY['message.send'])", tokenHash(legacyToken)); err != nil {
		t.Fatal(err)
	}
	config := a.C
	config.SMTPAddress, config.SMTPUser, config.SMTPPassword = "saved.example.test:587", "synthetic-user", "synthetic-upgrade-secret"
	config.SMTPTLS = "starttls"
	config.BounceDomain = "bounce.example.test"
	if err := a.saveRuntime(ctx, "smtp", config); err != nil {
		t.Fatal(err)
	}
	var runtimeBefore string
	if err := a.DB.QueryRow(ctx, "SELECT runtime_config::text FROM platform_settings WHERE id=1").Scan(&runtimeBefore); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runtimeBefore, config.SMTPPassword) {
		t.Fatal("upgrade fixture exposed its stored SMTP secret")
	}
	input := SendInput{ApplicationID: "app_demo", To: "released@example.test", Template: "welcome", Version: 2, Variables: map[string]string{"name": "Synthetic"}, Class: "transactional"}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, `INSERT INTO messages(id,application_id,recipient,from_address,subject,html,plain,class,idempotency_key,payload_hash,expires_at,envelope_sender)
		VALUES('msg_released','app_demo',$1,'notifications@example.test','Released version Synthetic','<p>Released Synthetic</p>','Released Synthetic','transactional','released-idempotency',$2,now()+interval '2 days','bounce-released@bounce.example.test')`, input.To, tokenHash(string(payload))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(ctx, "INSERT INTO delivery_events(message_id,status,detail,event_key) VALUES('msg_released','queued','Released admission','released-event')"); err != nil {
		t.Fatal(err)
	}

	for n := 0; n < 2; n++ {
		if err := a.Migrate(ctx); err != nil {
			t.Fatal("upgrade or repeat migration failed", err)
		}
	}
	a.C.RootPassword = "different-bootstrap-password-must-not-replace-root"
	if err := a.Seed(ctx, true); err != nil {
		t.Fatal(err)
	}
	var savedHash, runtimeAfter string
	a.DB.QueryRow(ctx, "SELECT password_hash FROM users WHERE id=$1", root).Scan(&savedHash)
	a.DB.QueryRow(ctx, "SELECT legacy_relay_backup::text FROM platform_settings WHERE id=1").Scan(&runtimeAfter)
	if savedHash != rootHash || !checkPassword(savedHash, "synthetic-upgrade-root-password") {
		t.Fatal("migration or repeat seeding changed the root password or saved settings")
	}
	var before, after RuntimeSettings
	json.Unmarshal([]byte(runtimeBefore), &before)
	json.Unmarshal([]byte(runtimeAfter), &after)
	if before.SMTP == nil || after.SMTP == nil || *before.SMTP != *after.SMTP {
		t.Fatal("encrypted SMTP settings were not copied exactly")
	}
	loaded, err := a.relayConfig(ctx, importedPrimaryRelay, "notifications@example.test", a.C)
	if err != nil || loaded.SMTPAddress != config.SMTPAddress || loaded.SMTPPassword != config.SMTPPassword || loaded.SMTPUser != config.SMTPUser || loaded.BounceDomain != config.BounceDomain {
		t.Fatal("saved encrypted SMTP configuration did not survive the upgrade")
	}
	var strict bool
	if err := a.DB.QueryRow(ctx, "SELECT strict_sender_domains FROM platform_settings WHERE id=1").Scan(&strict); err != nil || !strict {
		t.Fatal("upgrade did not require named sender routing", err)
	}
	if route, err := a.senderRelay(ctx, "notifications@example.test"); err != nil || route != importedPrimaryRelay {
		t.Fatal("legacy sender no longer resolves to its saved default SMTP configuration", route, err)
	}
	var route, envelope, state, hash, subject string
	var namedRelay, fallbackRelay *string
	if err := a.DB.QueryRow(ctx, "SELECT route,relay_id,fallback_relay_id,envelope_sender,status,payload_hash,subject FROM messages WHERE id='msg_released'").Scan(&route, &namedRelay, &fallbackRelay, &envelope, &state, &hash, &subject); err != nil {
		t.Fatal(err)
	}
	if route != "primary" || (namedRelay == nil || *namedRelay != importedPrimaryRelay) || fallbackRelay != nil || envelope != "bounce-released@bounce.example.test" || state != "queued" || hash != tokenHash(string(payload)) || subject != "Released version Synthetic" {
		t.Fatal("upgrade changed a pending message, envelope, route, or idempotency payload")
	}
	var version, rowVersion, events int
	var campaignState, dispatch string
	a.DB.QueryRow(ctx, "SELECT current_version FROM templates WHERE id='tpl_welcome'").Scan(&version)
	a.DB.QueryRow(ctx, "SELECT c.state,r.template_version,r.dispatch_key FROM campaigns c JOIN campaign_rows r ON r.campaign_id=c.id WHERE c.id='cmp_released'").Scan(&campaignState, &rowVersion, &dispatch)
	a.DB.QueryRow(ctx, "SELECT count(*) FROM delivery_events WHERE message_id='msg_released' AND event_key='released-event'").Scan(&events)
	if version != 2 || rowVersion != 1 || dispatch != "released-dispatch" || campaignState != "approved" || events != 1 {
		t.Fatal("upgrade rewrote frozen templates, campaign rows, or delivery evidence")
	}
	var ordinal int
	var sourceOrdinals []int
	if err := a.DB.QueryRow(ctx, "SELECT ordinal,source_ordinals FROM campaign_rows WHERE campaign_id='cmp_released'").Scan(&ordinal, &sourceOrdinals); err != nil || len(sourceOrdinals) != 1 || sourceOrdinals[0] != ordinal {
		t.Fatal("upgrade lost the released campaign's source row", err, sourceOrdinals)
	}
	var messageCC, campaignCC []string
	if err := a.DB.QueryRow(ctx, "SELECT cc FROM messages WHERE id='msg_released'").Scan(&messageCC); err != nil || len(messageCC) != 0 {
		t.Fatal("upgrade added unexpected message copies", err)
	}
	if err := a.DB.QueryRow(ctx, "SELECT cc FROM campaigns WHERE id='cmp_released'").Scan(&campaignCC); err != nil || len(campaignCC) != 0 {
		t.Fatal("upgrade added unexpected campaign copies", err)
	}
	var owner *string
	if err := a.DB.QueryRow(ctx, "SELECT created_by FROM api_keys WHERE id='key_released'").Scan(&owner); err != nil || owner != nil {
		t.Fatal("upgrade assigned a false human identity to a legacy service key", err)
	}
	entries, err := resources.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var migrations int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrations)
	if migrations != len(entries) {
		t.Fatalf("recorded migrations=%d, embedded migrations=%d", migrations, len(entries))
	}
	request := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+legacyToken)
	request.Header.Set("Idempotency-Key", "released-idempotency")
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	result := automationObject(t, response, 202)
	if result["id"] != "msg_released" || result["replayed"] != true {
		t.Fatal("legacy API credential or original idempotency key stopped working")
	}
	var messages int
	a.DB.QueryRow(ctx, "SELECT count(*) FROM messages").Scan(&messages)
	if messages != 1 {
		t.Fatal("upgrade retry duplicated a durable message")
	}
	// The latest schema also accepts new events without introducing historical
	// webhook jobs for endpoints which did not exist when those events occurred.
	if _, err := a.DB.Exec(ctx, "INSERT INTO delivery_events(message_id,status,detail,event_key) VALUES('msg_released','relay_accepted','Synthetic gateway acceptance','released-second-event')"); err != nil {
		t.Fatal("upgraded delivery event trigger failed", err)
	}
}
