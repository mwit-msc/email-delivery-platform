package edp

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql seeds/*.json web/assets/* web/views/*
var resources embed.FS

type App struct {
	DB      *pgxpool.Pool
	C       Config
	Version string
}

func New(ctx context.Context, c Config, version string) (*App, error) {
	p, e := pgxpool.New(ctx, c.DatabaseURL)
	if e != nil {
		return nil, e
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return &App{DB: p, C: c, Version: version}, nil
}
func (a *App) Close() { a.DB.Close() }
func (a *App) Migrate(ctx context.Context) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(824192)"); e != nil {
		return e
	}
	var exists bool
	e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='schema_migrations')").Scan(&exists)
	if e != nil {
		return e
	}
	if !exists {
		data, _ := resources.ReadFile("migrations/001_initial.sql")
		if _, e = tx.Exec(ctx, string(data)); e != nil {
			return e
		}
	}
	entries, e := resources.ReadDir("migrations")
	if e != nil {
		return e
	}
	for _, entry := range entries {
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			return err
		}
		var applied bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", version).Scan(&applied); e != nil {
			return e
		}
		if !applied {
			data, err := resources.ReadFile("migrations/" + entry.Name())
			if err != nil {
				return err
			}
			if _, e = tx.Exec(ctx, string(data)); e != nil {
				return e
			}
			if version == 13 {
				if e = a.migrateLegacyRelays(ctx, tx); e != nil {
					return e
				}
			}
		}
	}
	return tx.Commit(ctx)
}
func (a *App) audit(ctx context.Context, actor, action, scope, detail string) {
	a.DB.Exec(ctx, "INSERT INTO audit_logs(actor,action,scope,detail) VALUES($1,$2,$3,$4)", actor, action, scope, detail)
}
func (a *App) Seed(ctx context.Context, demo bool) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(824193)"); e != nil {
		return e
	}
	var hasRoot bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE root)").Scan(&hasRoot); e != nil {
		return e
	}
	if !hasRoot {
		hash := a.C.RootHash
		if hash == "" {
			if len(a.C.RootPassword) < 12 {
				return fmt.Errorf("initial ROOT_PASSWORD must contain at least 12 characters, or supply ROOT_PASSWORD_HASH")
			}
			hash, e = hashPassword(a.C.RootPassword)
			if e != nil {
				return e
			}
		} else if !validPasswordHash(hash) {
			return fmt.Errorf("ROOT_PASSWORD_HASH must be a valid Argon2id hash")
		}
		if _, e = tx.Exec(ctx, "INSERT INTO users(id,username,name,password_hash,root) VALUES($1,$2,'Platform administrator',$3,true)", newID("usr_"), a.C.RootUsername, hash); e != nil {
			return e
		}
	}
	if demo {
		name, sender := "Demo application", "notifications@example.test"

		if _, e = tx.Exec(ctx, "INSERT INTO applications(id,name,from_address,daily_limit,recipient_hour_limit) VALUES('app_demo',$1,$2,5000,20) ON CONFLICT(id) DO NOTHING", name, sender); e != nil {
			return e
		}
		if e = seedDemoRelay(ctx, tx); e != nil {
			return e
		}
		var cases []struct{ Event, Award, Label, Subject, HTML, Text string }
		if a.C.SeedCertificatesFile != "" {
			data, err := os.ReadFile(a.C.SeedCertificatesFile)
			if err != nil {
				return fmt.Errorf("read seed certificates: %w", err)
			}
			if e = json.Unmarshal(data, &cases); e != nil {
				return fmt.Errorf("decode seed certificates: %w", e)
			}
		}
		for _, t := range cases {
			slug := t.Event + "." + t.Award
			id := "tpl_" + strings.ReplaceAll(slug, ".", "_")
			html := strings.ReplaceAll(t.HTML, "{{logo_url}}", a.C.PublicURL+"/demo/logo.png")
			if _, e = tx.Exec(ctx, "INSERT INTO templates(id,application_id,slug,name) VALUES($1,'app_demo',$2,$3) ON CONFLICT(application_id,slug) DO NOTHING", id, slug, t.Label); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO template_versions(template_id,version,subject,html,plain,variables) VALUES($1,1,$2,$3,$4,ARRAY['school_name','certificate_url']) ON CONFLICT DO NOTHING", id, t.Subject, html, t.Text); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "INSERT INTO templates(id,application_id,slug,name) VALUES('tpl_welcome','app_demo','welcome','Welcome message') ON CONFLICT DO NOTHING"); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO template_versions(template_id,version,subject,html,plain,variables) VALUES('tpl_welcome',1,'Welcome, {{name}}','<p>Hello {{name}},</p><p>Your registration is confirmed.</p>','Hello {{name}}, your registration is confirmed.',ARRAY['name']) ON CONFLICT DO NOTHING"); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, "INSERT INTO audit_logs(actor,action,detail) VALUES('system','seed',$1)", fmt.Sprintf("demo=%t", demo)); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) ResetRoot(ctx context.Context) error {
	if len(a.C.RootPassword) < 12 {
		return fmt.Errorf("ROOT_PASSWORD must contain at least 12 characters")
	}
	hash, e := hashPassword(a.C.RootPassword)
	if e != nil {
		return e
	}
	tag, e := a.DB.Exec(ctx, "UPDATE users SET password_hash=$1,disabled=false WHERE root", hash)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("root does not exist; seed first")
	}
	_, e = a.DB.Exec(ctx, "DELETE FROM sessions WHERE user_id IN(SELECT id FROM users WHERE root)")
	a.audit(ctx, "system", "root.password.reset", "*", "")
	return e
}
