package edp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
)

type TemplateInput struct {
	Slug    string          `json:"slug"`
	Name    string          `json:"name"`
	Subject string          `json:"subject"`
	HTML    string          `json:"html"`
	Plain   string          `json:"plain"`
	Project json.RawMessage `json:"project"`
	Version int             `json:"version"`
}

func prepareTemplate(in TemplateInput) (TemplateInput, []string, error) {
	if in.Subject == "" || len(in.Subject) > 998 || strings.ContainsAny(in.Subject, "\r\n") || len(in.HTML) > 2<<20 || len(in.Plain) > 2<<20 || len(in.Project) > 2<<20 {
		return in, nil, fmt.Errorf("invalid template content or size")
	}
	if len(in.Project) == 0 {
		in.Project = json.RawMessage(`{}`)
	}
	var err error
	in.Project, err = cleanProject(in.Project)
	if err != nil {
		return in, nil, fmt.Errorf("invalid editor project")
	}
	in.HTML = cleanHTML(in.HTML)
	if in.Plain == "" {
		in.Plain = plainHTML(in.HTML)
	}
	vars := []string{}
	for _, m := range placeholders.FindAllStringSubmatch(in.Subject+in.HTML+in.Plain, -1) {
		if !contains(vars, m[1]) {
			vars = append(vars, m[1])
		}
	}
	return in, vars, nil
}

func (a *App) createTemplate(ctx context.Context, app string, in TemplateInput) (string, error) {
	if !regexpSlug(in.Slug) || strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 {
		return "", fmt.Errorf("a name and valid unique slug are required")
	}
	in, vars, e := prepareTemplate(in)
	if e != nil {
		return "", e
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	id := newID("tpl_")
	if _, e = tx.Exec(ctx, "INSERT INTO templates(id,application_id,slug,name) VALUES($1,$2,$3,$4)", id, app, in.Slug, in.Name); e != nil {
		return "", fmt.Errorf("template slug must be unique within the application")
	}
	if _, e = tx.Exec(ctx, "INSERT INTO template_versions(template_id,version,subject,html,plain,variables,project) VALUES($1,1,$2,$3,$4,$5,$6)", id, in.Subject, in.HTML, in.Plain, vars, []byte(in.Project)); e != nil {
		return "", e
	}
	return id, tx.Commit(ctx)
}

func (a *App) saveTemplateVersion(ctx context.Context, app, id string, in TemplateInput) (int, []string, error) {
	in, vars, e := prepareTemplate(in)
	if e != nil {
		return 0, nil, e
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return 0, nil, e
	}
	defer tx.Rollback(ctx)
	var current int
	if e = tx.QueryRow(ctx, "SELECT current_version FROM templates WHERE id=$1 AND application_id=$2 FOR UPDATE", id, app).Scan(&current); e != nil {
		return 0, nil, fmt.Errorf("template not found")
	}
	if current != in.Version {
		return 0, nil, fmt.Errorf("template changed in another session; reload before saving")
	}
	if _, e = tx.Exec(ctx, "INSERT INTO template_versions(template_id,version,subject,html,plain,variables,project) VALUES($1,$2,$3,$4,$5,$6,$7)", id, current+1, in.Subject, in.HTML, in.Plain, vars, []byte(in.Project)); e != nil {
		return 0, nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE templates SET current_version=$2 WHERE id=$1", id, current+1); e != nil {
		return 0, nil, e
	}
	return current + 1, vars, tx.Commit(ctx)
}

func renderTemplateInput(in TemplateInput, vars map[string]string) (Rendered, error) {
	in, _, e := prepareTemplate(in)
	if e != nil {
		return Rendered{}, e
	}
	var out Rendered
	out.Subject, e = renderVariables(in.Subject, vars, false)
	if e != nil {
		return out, e
	}
	if len(out.Subject) > 998 || strings.ContainsAny(out.Subject, "\r\n") {
		return out, fmt.Errorf("invalid rendered subject")
	}
	out.HTML, e = renderVariables(in.HTML, vars, true)
	if e != nil {
		return out, e
	}
	out.HTML = cleanHTML(out.HTML)
	out.Plain, e = renderVariables(in.Plain, vars, false)
	return out, e
}

func (a *App) previewTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, e := a.one(r.Context(), "SELECT application_id FROM templates WHERE id=$1", id)
	if e != nil {
		problem(w, 404, "template not found")
		return
	}
	app := row["application_id"].(string)
	if !a.require(w, r, app, "template.view") {
		return
	}
	var in struct {
		Template  *TemplateInput    `json:"template"`
		Variables map[string]string `json:"variables"`
		Version   int               `json:"version"`
	}
	if !decodeAPI(w, r, &in) {
		return
	}
	var rendered Rendered
	if in.Template != nil {
		rendered, e = renderTemplateInput(*in.Template, in.Variables)
	} else {
		rendered, e = a.renderMessage(r.Context(), app, id, in.Version, in.Variables)
	}
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	respond(w, 200, map[string]any{"subject": rendered.Subject, "html": rendered.HTML, "plain": rendered.Plain})
}

func (a *App) uploadEmailAsset(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("id")
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		if person(r) == nil {
			problem(w, 401, "authentication required")
			return
		}
		app = person(r).Scope
	}
	if !a.require(w, r, app, "template.edit") {
		return
	}
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		problem(w, 400, "invalid image upload")
		return
	}
	f, h, e := r.FormFile("file")
	if e != nil {
		problem(w, 400, "choose a PNG, JPEG, or GIF image")
		return
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		problem(w, 400, "image exceeds 1 MiB")
		return
	}
	dimensions, format, e := image.DecodeConfig(bytes.NewReader(b))
	if e != nil || dimensions.Width < 1 || dimensions.Height < 1 || int64(dimensions.Width)*int64(dimensions.Height) > 16_000_000 {
		problem(w, 400, "invalid image or image dimensions exceed 16 megapixels")
		return
	}
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}[format]
	if mime == "" {
		problem(w, 400, "unsupported image type")
		return
	}
	var count int
	tx, e := a.DB.Begin(r.Context())
	if e != nil {
		problem(w, 500, "asset storage unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	if _, e = tx.Exec(r.Context(), "SELECT id FROM applications WHERE id=$1 FOR UPDATE", app); e != nil {
		problem(w, 400, "application not found")
		return
	}
	if e = tx.QueryRow(r.Context(), "SELECT count(*) FROM email_assets WHERE application_id=$1", app).Scan(&count); e != nil || count >= 100 {
		problem(w, 400, "application image limit is 100")
		return
	}
	id := newID("asset_")
	name := h.Filename
	if len(name) > 200 {
		name = name[:200]
	}
	if _, e = tx.Exec(r.Context(), "INSERT INTO email_assets(id,application_id,name,mime_type,data) VALUES($1,$2,$3,$4,$5)", id, app, name, mime, b); e != nil {
		problem(w, 500, "could not store image")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		problem(w, 500, "could not store image")
		return
	}
	config, e := a.runtimeConfig(r.Context())
	if e != nil {
		problem(w, 500, "runtime configuration unavailable")
		return
	}
	a.audit(r.Context(), person(r).ID, "template.asset", app, id)
	respond(w, 201, map[string]any{"id": id, "url": strings.TrimRight(config.PublicURL, "/") + "/email-assets/" + id, "mime_type": mime})
}

func (a *App) emailAsset(w http.ResponseWriter, r *http.Request) {
	var b []byte
	var mime string
	if e := a.DB.QueryRow(r.Context(), "SELECT data,mime_type FROM email_assets WHERE id=$1", r.PathValue("id")).Scan(&b, &mime); e != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Write(b)
}
