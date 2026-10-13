package edp

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func normalizeCC(to string, input []string) ([]string, error) {
	if len(input) > 20 {
		return nil, fmt.Errorf("at most 20 CC addresses")
	}
	seen := map[string]bool{strings.ToLower(to): true}
	out := []string{}
	for _, address := range input {
		address = certificateRecipientKey(strings.TrimSpace(address))
		if !validAddress(address) {
			return nil, fmt.Errorf("invalid CC address")
		}
		key := strings.ToLower(address)
		if !seen[key] {
			seen[key] = true
			out = append(out, address)
		}
	}
	sort.Strings(out)
	return out, nil
}

func parseCC(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
}

// Each copy consumes recipient capacity, while a shared body is one queue job.
func checkRecipientCapacity(ctx context.Context, tx pgx.Tx, app string, additions map[string]int, limit int) error {
	for recipient, added := range additions {
		var used int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE application_id=$1 AND created_at>=now()-interval '1 hour' AND (lower(recipient)=lower($2) OR EXISTS(SELECT 1 FROM unnest(cc) address WHERE lower(address)=lower($2)))`, app, recipient).Scan(&used); err != nil {
			return err
		}
		if used+added > limit {
			return fmt.Errorf("recipient cooldown limit reached (including CC)")
		}
	}
	return nil
}

func (a *App) setCampaignCC(ctx context.Context, id string, input []string) error {
	cc, err := normalizeCC("", input)
	if err != nil {
		return err
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err = tx.QueryRow(ctx, "SELECT state FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&state); err != nil {
		return err
	}
	if state != "draft" {
		return fmt.Errorf("CC recipients are frozen; return an unsent campaign to draft before editing")
	}
	if _, err = tx.Exec(ctx, "UPDATE campaigns SET cc=$2 WHERE id=$1", id, cc); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *App) apiCampaignCC(w http.ResponseWriter, r *http.Request) {
	app, _, ok := a.scopedCampaign(w, r, "campaign.edit")
	if !ok {
		return
	}
	var in struct {
		CC *[]string `json:"cc"`
	}
	if !decodeAPI(w, r, &in) {
		return
	}
	if in.CC == nil {
		problem(w, 400, "cc must be an array; use [] to clear")
		return
	}
	if err := a.setCampaignCC(r.Context(), r.PathValue("id"), *in.CC); err != nil {
		problem(w, 400, err.Error())
		return
	}
	a.audit(r.Context(), person(r).ID, "campaign.cc", app, r.PathValue("id"))
	respond(w, 200, map[string]any{"saved": true})
}
