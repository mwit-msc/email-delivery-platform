package edp

import (
	"net/http"
	"net/url"
	"strings"
)

func (a *App) archiveTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var app string
	if err := a.DB.QueryRow(r.Context(), "SELECT application_id FROM templates WHERE id=$1", id).Scan(&app); err != nil {
		problem(w, 404, "template not found")
		return
	}
	api := strings.HasPrefix(r.URL.Path, "/v1/")
	if api && person(r) == nil {
		problem(w, 401, "authentication required")
		return
	}
	if api && person(r).Scope != app {
		problem(w, 404, "template not found")
		return
	}
	if !a.require(w, r, app, "template.edit") {
		return
	}
	archived := false
	if api {
		var in struct {
			Archived *bool `json:"archived"`
		}
		if !decodeAPI(w, r, &in) {
			return
		}
		if in.Archived == nil {
			problem(w, 400, "archived is required")
			return
		}
		archived = *in.Archived
	} else {
		r.ParseForm()
		archived = r.FormValue("archived") == "true"
	}
	if _, err := a.DB.Exec(r.Context(), "UPDATE templates SET archived=$2 WHERE id=$1 AND application_id=$3", id, archived, app); err != nil {
		problem(w, 500, "template archive state could not be saved")
		return
	}
	act := "template.restore"
	if archived {
		act = "template.archive"
	}
	a.audit(r.Context(), person(r).ID, act, app, id)
	if api {
		respond(w, 200, map[string]any{"id": id, "archived": archived})
		return
	}
	query := url.Values{"app": {app}}
	if r.FormValue("include_archived") == "true" {
		query.Set("include_archived", "true")
	}
	http.Redirect(w, r, "/templates?"+query.Encode(), http.StatusSeeOther)
}
