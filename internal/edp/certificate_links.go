package edp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"
)

type drivePDF struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Text        string `json:"-"`
	Method      string `json:"method,omitempty"`
	Pages       int    `json:"pages,omitempty"`
	Quality     int    `json:"quality,omitempty"`
	Error       string `json:"error,omitempty"`
	ContentRead bool   `json:"content_read,omitempty"`
}

func (f drivePDF) link() string {
	return "https://drive.google.com/file/d/" + url.PathEscape(f.ID) + "/view"
}

type certificateMatch struct {
	Row        int        `json:"row"`
	FileID     string     `json:"file_id"`
	Score      int        `json:"score"`
	Reason     string     `json:"reason"`
	Ambiguous  bool       `json:"ambiguous"`
	Status     string     `json:"status"`
	Method     string     `json:"method,omitempty"`
	Evidence   string     `json:"evidence,omitempty"`
	ReviewedBy string     `json:"reviewed_by,omitempty"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
	Manual     bool       `json:"manual,omitempty"`
}
type certificateLinks struct {
	GoogleUser       string             `json:"google_user,omitempty"`
	GoogleConnection string             `json:"google_connection,omitempty"`
	ScanID           string             `json:"scan_id,omitempty"`
	ScanState        string             `json:"scan_state,omitempty"`
	ForceOCR         bool               `json:"force_ocr,omitempty"`
	AutoThreshold    int                `json:"auto_threshold,omitempty"`
	Actor            string             `json:"actor,omitempty"`
	Folder           string             `json:"folder"`
	URLColumn        int                `json:"url_column"`
	Columns          []int              `json:"columns"`
	Files            []drivePDF         `json:"files"`
	Matches          []certificateMatch `json:"matches"`
}

var googleFileID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,150}$`)

func driveFolderID(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host != "drive.google.com" || u.User != nil {
		return "", fmt.Errorf("use a Google Drive HTTPS folder link")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "folders" && i+2 == len(parts) && googleFileID.MatchString(parts[i+1]) {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("use a Drive folder link ending in /folders/FOLDER_ID")
}
func (a *App) drivePDFs(ctx context.Context, link string, identity ...string) ([]drivePDF, error) {
	id, err := driveFolderID(link)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	user, connection := "", ""
	if len(identity) == 2 {
		user, connection = identity[0], identity[1]
	}
	client, err := a.googleReadClient(ctx, user, connection)
	if err != nil {
		return nil, err
	}

	return listDrivePDFs(ctx, client, id)
}

// Only metadata is read, from a fixed Google endpoint. No links in PDFs are fetched.
func listDrivePDFs(ctx context.Context, client *http.Client, id string) ([]drivePDF, error) {
	files := []drivePDF{}
	token := ""
	seen := map[string]bool{}
	for page := 0; page < 50; page++ {
		q := url.Values{"q": {"'" + id + "' in parents and trashed = false and mimeType = 'application/pdf'"}, "fields": {"nextPageToken,incompleteSearch,files(id,name)"}, "pageSize": {"1000"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}, "orderBy": {"name"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		req, err := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/drive/v3/files?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Google Drive could not be reached")
		}
		var out struct {
			Files      []drivePDF `json:"files"`
			Next       string     `json:"nextPageToken"`
			Incomplete bool       `json:"incompleteSearch"`
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, fmt.Errorf("Google Drive returned %d; enable the Drive API and check read access for the connected account", res.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&out)
		res.Body.Close()
		if err != nil || out.Incomplete {
			return nil, fmt.Errorf("Google Drive returned an incomplete file listing; narrow the folder and retry")
		}
		for _, f := range out.Files {
			if !googleFileID.MatchString(f.ID) || len(f.Name) > 4096 {
				return nil, fmt.Errorf("invalid Drive file metadata")
			}
			if !seen[f.ID] {
				files = append(files, f)
				seen[f.ID] = true
			}
		}
		if len(files) > 5000 {
			return nil, fmt.Errorf("folder exceeds 5000 PDFs; use a smaller folder")
		}
		if out.Next == "" {
			return files, nil
		}
		if out.Next == token {
			return nil, fmt.Errorf("Google Drive repeated a page token")
		}
		token = out.Next
	}
	return nil, fmt.Errorf("Google Drive listing exceeds the page limit")
}
func matchText(s string) string {
	s = strings.ToLower(norm.NFKC.String(strings.TrimSuffix(strings.TrimSuffix(s, ".pdf"), ".PDF")))
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) }), " ")
}

type filenameGrams struct {
	Counts map[string]int
	Length int
}

func filenameBigrams(value string) filenameGrams {
	runes := []rune(value)
	out := filenameGrams{Counts: map[string]int{}, Length: len(runes)}
	for i := 1; i < len(runes); i++ {
		out.Counts[string(runes[i-1:i+1])]++
	}
	return out
}
func gramSimilarity(a, b filenameGrams) int {
	if a.Length < 4 || b.Length < 4 {
		return 0
	}
	hits := 0
	for gram, count := range a.Counts {
		hits += min(count, b.Counts[gram])
	}
	return 2 * hits * 79 / (a.Length + b.Length - 2)
}
func fuzzySimilarity(a, b string) int { return gramSimilarity(filenameBigrams(a), filenameBigrams(b)) }
func matchCertificates(rows [][]string, files []drivePDF, columns []int, urlColumn int) []certificateMatch {
	out := make([]certificateMatch, 0, len(rows)-1)
	names := make([]string, len(files))
	grams := make([]filenameGrams, len(files))
	words := map[string][]int{}
	for i, f := range files {
		names[i] = matchText(f.Name)
		grams[i] = filenameBigrams(names[i])
		seen := map[string]bool{}
		for _, word := range strings.Fields(names[i]) {
			if !seen[word] {
				words[word] = append(words[word], i)
				seen[word] = true
			}
		}
	}
	for i, row := range rows[1:] {
		m := certificateMatch{Row: i + 2, Status: "pending", Reason: "No filename matched the selected identity columns"}
		if cell(row, urlColumn) != "" {
			for _, f := range files {
				if cell(row, urlColumn) == f.link() {
					m.FileID = f.ID
					break
				}
			}
			m.Status = "existing"
			m.Reason = "Existing link retained"
			out = append(out, m)
			continue
		}
		keys := []string{}
		for _, c := range columns {
			if k := matchText(cell(row, c)); len([]rune(k)) >= 3 {
				keys = append(keys, k)
			}
		}
		candidates := map[int]bool{}
		for _, key := range keys {
			var shortlist []int
			for _, word := range strings.Fields(key) {
				if hits := words[word]; len(hits) > 0 && (shortlist == nil || len(hits) < len(shortlist)) {
					shortlist = hits
				}
			}
			if len(shortlist) <= 250 {
				for _, index := range shortlist {
					candidates[index] = true
				}
			}
		}
		// Small folders can also offer spelling suggestions without shared words.
		if len(files) <= 250 {
			for j := range files {
				candidates[j] = true
			}
		}
		best, runner := 0, 0
		for j, name := range names {
			if !candidates[j] {
				continue
			}
			exact := 0
			for _, k := range keys {
				if strings.Contains(" "+name+" ", " "+k+" ") {
					exact++
				}
			}
			score := 0
			reason := ""
			if exact > 0 {
				score = 80 + 15*exact/max(1, len(keys))
				if exact == len(keys) && len(keys) > 1 {
					score = 100
				}
				reason = fmt.Sprintf("Exact filename evidence for %d of %d identity values", exact, len(keys))
			}
			if score > best {
				runner = best
				best = score
				m.FileID = files[j].ID
				m.Score = score
				m.Reason = reason
			} else if score > runner {
				runner = score
			}
		}
		// Exact evidence outranks every spelling similarity. Only run fuzzy
		// comparisons when exact matching found nothing, reusing filename grams.
		if best == 0 && len(keys) > 0 {
			identity := filenameBigrams(strings.Join(keys, " "))
			for j, filename := range grams {
				if !candidates[j] {
					continue
				}
				score := gramSimilarity(identity, filename)
				if score > best {
					runner = best
					best = score
					m.FileID = files[j].ID
					m.Score = score
					m.Reason = "Similar filename spelling; check the PDF manually"
				} else if score > runner {
					runner = score
				}
			}
		}
		if len(keys) < len(columns) && m.Score > 89 {
			m.Score = 89
			m.Reason += "; some identity cells are blank or too short"
		}
		if best > 0 && best-runner < 10 {
			m.Ambiguous = true
			m.Reason += "; multiple filenames score similarly"
		}
		out = append(out, m)
	}
	uses := map[string]int{}
	for _, m := range out {
		if m.FileID != "" {
			uses[m.FileID]++
		}
	}
	for i := range out {
		if uses[out[i].FileID] > 1 {
			out[i].Ambiguous = true
			out[i].Reason += "; this PDF is suggested for multiple rows"
		}
	}
	return out
}
func sourceRevision(rows [][]string) string { b, _ := json.Marshal(rows); return tokenHash(string(b)) }
func decodeLinks(raw []byte) (certificateLinks, error) {
	var state certificateLinks
	err := json.Unmarshal(raw, &state)
	return state, err
}
func campaignLinkErrors(ctx context.Context, tx pgx.Tx, id string, rows [][]string) (map[int]string, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT certificate_links FROM campaigns WHERE id=$1", id).Scan(&raw); err != nil {
		return nil, err
	}
	state, err := decodeLinks(raw)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	files := map[string]drivePDF{}
	for _, f := range state.Files {
		files[f.ID] = f
	}
	for _, m := range state.Matches {
		if m.Status == "existing" {
			continue
		}
		if m.Status != "accepted" || m.Row < 2 || m.Row > len(rows) || cell(rows[m.Row-1], state.URLColumn) != files[m.FileID].link() {
			out[m.Row] = "Certificate link needs review"
		}
	}
	return out, nil
}
func requireReviewedLinks(ctx context.Context, tx pgx.Tx, id string) error {
	var scanState string
	if err := tx.QueryRow(ctx, "SELECT coalesce(certificate_links->>'scan_state','') FROM campaigns WHERE id=$1", id).Scan(&scanState); err != nil {
		return err
	}
	if scanState == "reading" {
		return fmt.Errorf("wait for the local certificate scan before approving")
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT source_rows FROM campaigns WHERE id=$1", id).Scan(&raw); err != nil {
		return err
	}
	var source [][]string
	if err := json.Unmarshal(raw, &source); err != nil {
		return err
	}
	errors, err := campaignLinkErrors(ctx, tx, id, source)
	if err != nil || len(errors) == 0 {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT ordinal,source_ordinals FROM campaign_rows WHERE campaign_id=$1 AND NOT excluded", id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var sources []int
		if err := rows.Scan(&ordinal, &sources); err != nil {
			return err
		}
		if len(sources) == 0 {
			sources = []int{ordinal}
		}
		for _, n := range sources {
			if errors[n] != "" {
				return fmt.Errorf("review certificate links or exclude their rows before campaign approval")
			}
		}
	}
	return rows.Err()
}

// Edits and link decisions serialize with approval and remapping. A revision
// prevents browser tabs from overwriting a newer source or review decision.
func (a *App) campaignWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := a.one(r.Context(), "SELECT * FROM campaigns WHERE id=$1", id)
	if err != nil {
		problem(w, 404, "campaign not found")
		return
	}
	app := data["application_id"].(string)
	permission := "campaign.view"
	if r.Method == "POST" {
		permission = "campaign.edit"
	}
	if !a.require(w, r, app, permission) {
		return
	}
	if r.Method == "GET" {
		raw, _ := json.Marshal(data["certificate_links"])
		links, _ := decodeLinks(raw)
		progress := a.certificateScanProgress(r.Context(), id, links.ScanID)
		if r.URL.Query().Get("progress") == "1" {
			respond(w, 200, map[string]any{"scan_state": links.ScanState, "progress": progress, "revision": workspaceDataRevision(data)})
			return
		}
		respond(w, 200, map[string]any{"rows": data["source_rows"], "links": data["certificate_links"], "mapping": data["mapping"], "revision": workspaceDataRevision(data), "progress": progress})
		return
	}
	var in struct {
		Action     string     `json:"action"`
		Revision   string     `json:"revision"`
		Rows       [][]string `json:"rows"`
		Folder     string     `json:"folder"`
		Columns    []int      `json:"columns"`
		URLColumn  int        `json:"url_column"`
		Threshold  int        `json:"threshold"`
		ForceOCR   bool       `json:"force_ocr"`
		AutoAccept bool       `json:"auto_accept"`
		Decisions  []struct {
			Row    int    `json:"row"`
			FileID string `json:"file_id"`
			Accept bool   `json:"accept"`
		} `json:"decisions"`
	}
	if !decodeAPI(w, r, &in) {
		return
	}
	var files []drivePDF
	var googleUser, googleConnection string
	if in.Action == "match" {
		if data["kind"] != "certificate" || data["state"] != "draft" {
			problem(w, 400, "match PDFs in a draft certificate campaign")
			return
		}
		if person(r).KeyID == "" {
			googleUser, googleConnection, err = a.googleReadIdentity(r.Context(), person(r).ID)
		}
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
		files, err = a.drivePDFs(r.Context(), in.Folder, googleUser, googleConnection)
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
	}
	tx, err := a.DB.Begin(r.Context())
	if err != nil {
		problem(w, 500, "workspace unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var state, kind string
	var raw, linkRaw, mappingRaw []byte
	err = tx.QueryRow(r.Context(), "SELECT state,kind,source_rows,certificate_links,mapping FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&state, &kind, &raw, &linkRaw, &mappingRaw)
	if err != nil {
		problem(w, 500, "workspace unavailable")
		return
	}
	if state != "draft" {
		problem(w, 409, "return the campaign to draft before editing")
		return
	}
	var rows [][]string
	json.Unmarshal(raw, &rows)
	links, err := decodeLinks(linkRaw)
	if err != nil {
		problem(w, 500, "link review unavailable")
		return
	}
	revision := workspaceRevision(rows, links)
	if revision != in.Revision {
		problem(w, 409, "the sheet or link review changed; reload before saving")
		return
	}
	switch in.Action {
	case "edit":
		_, err = normalizeImportRows(in.Rows)
		if len(in.Rows) > 5001 {
			err = fmt.Errorf("maximum 5000 recipient rows")
		}
		if err == nil && (len(in.Rows) < len(rows) || (len(rows) > 0 && len(in.Rows[0]) < len(rows[0]))) {
			err = fmt.Errorf("keep existing rows and columns; clear cells or exclude recipients instead")
		}
		if err == nil {
			for _, row := range in.Rows {
				if len(row) > len(in.Rows[0]) {
					err = fmt.Errorf("every cell needs a column header")
					break
				}
			}
		}
		if err == nil {
			if sourceRevision(in.Rows) != sourceRevision(rows) {
				// Any identity edit invalidates the previous filename evidence. Remove
				// generated URLs unless the operator explicitly replaced them in the grid.
				known := map[string]drivePDF{}
				for _, f := range links.Files {
					known[f.ID] = f
				}
				for _, m := range links.Matches {
					if m.Status == "accepted" && m.Row <= len(in.Rows) && cell(in.Rows[m.Row-1], links.URLColumn) == known[m.FileID].link() {
						in.Rows[m.Row-1][links.URLColumn] = ""
					}
				}
				rows = in.Rows
				links = certificateLinks{}
				_, err = tx.Exec(r.Context(), "DELETE FROM certificate_scan_files WHERE campaign_id=$1", id)
			}
		}
	case "match":
		if kind != "certificate" {
			err = fmt.Errorf("PDF matching requires a certificate campaign")
			break
		}
		if len(rows) < 2 || in.URLColumn < 0 || in.URLColumn >= len(rows[0]) || len(in.Columns) == 0 || len(in.Columns) > 52 {
			err = fmt.Errorf("choose a link column and identity columns")
			break
		}
		seen := map[int]bool{}
		for _, c := range in.Columns {
			if c < 0 || c >= len(rows[0]) || c == in.URLColumn || seen[c] {
				err = fmt.Errorf("choose distinct identity columns, excluding the link column")
				break
			}
			seen[c] = true
		}
		if err == nil {
			if in.AutoAccept && (in.Threshold < 1 || in.Threshold > 100) {
				err = fmt.Errorf("confidence threshold must be 1–100")
				break
			}
			links = certificateLinks{Folder: in.Folder, URLColumn: in.URLColumn, Columns: in.Columns, Files: files, Matches: matchCertificates(rows, files, in.Columns, in.URLColumn), ForceOCR: in.ForceOCR, Actor: person(r).ID, GoogleUser: googleUser, GoogleConnection: googleConnection}
			if in.AutoAccept {
				links.AutoThreshold = in.Threshold
			}
			err = queueCertificateScan(r.Context(), tx, id, &links)
		}
	case "cancel_scan":
		_, err = tx.Exec(r.Context(), "DELETE FROM certificate_scan_files WHERE campaign_id=$1", id)
		links.ScanState = "cancelled"
		for i := range links.Matches {
			if links.Matches[i].Status == "pending" {
				links.Matches[i].Ambiguous = true
				links.Matches[i].Score = min(links.Matches[i].Score, 69)
				links.Matches[i].Reason += "; local scan cancelled, manual review required"
			}
		}
	case "review", "auto":
		if links.ScanState == "reading" {
			err = fmt.Errorf("wait for local PDF extraction to finish or cancel the scan")
			break
		}
		known := map[string]drivePDF{}
		for _, f := range links.Files {
			known[f.ID] = f
		}
		if len(links.Matches) == 0 {
			err = fmt.Errorf("scan a Drive folder first")
			break
		}
		if in.Action == "auto" && (in.Threshold < 1 || in.Threshold > 100) {
			err = fmt.Errorf("confidence threshold must be 1–100")
			break
		}
		if len(in.Decisions) > 5000 {
			err = fmt.Errorf("too many review decisions")
			break
		}
		seenRows := map[int]bool{}
		decisions := map[int]struct {
			FileID string
			Accept bool
		}{}
		for _, d := range in.Decisions {
			if d.Row < 2 || d.Row > len(rows) || seenRows[d.Row] {
				err = fmt.Errorf("invalid review row")
				break
			}
			seenRows[d.Row] = true
			decisions[d.Row] = struct {
				FileID string
				Accept bool
			}{d.FileID, d.Accept}
		}
		used := map[string]bool{}
		for _, row := range rows[1:] {
			if link := cell(row, links.URLColumn); link != "" {
				used[link] = true
			}
		}
		for i := range links.Matches {
			m := &links.Matches[i]
			if m.Status == "existing" {
				continue
			}
			d, found := decisions[m.Row]
			if in.Action == "auto" {
				found = m.Status == "pending" && !m.Ambiguous && m.Score >= in.Threshold && m.FileID != "" && !used[known[m.FileID].link()]
				d.FileID = m.FileID
				d.Accept = true
			}
			if !found {
				continue
			}
			delete(seenRows, m.Row)
			if d.Accept {
				f, ok := known[d.FileID]
				if !ok {
					err = fmt.Errorf("choose a PDF from this folder")
					break
				}
				if in.Action == "review" && m.FileID != f.ID {
					m.Score = 0
					m.Method = "manual"
					m.Evidence = ""
					m.Ambiguous = true
					m.Reason = "Manually selected a different PDF; automatic confidence is unavailable"
				}
				m.FileID = f.ID
				m.Status = "accepted"
				m.Manual = in.Action == "review"
				m.ReviewedBy = person(r).ID
				now := time.Now().UTC()
				m.ReviewedAt = &now
				for len(rows[m.Row-1]) <= links.URLColumn {
					rows[m.Row-1] = append(rows[m.Row-1], "")
				}
				rows[m.Row-1][links.URLColumn] = f.link()
				used[f.link()] = true
			} else {
				if m.Status == "accepted" {
					rows[m.Row-1][links.URLColumn] = ""
				}
				m.Status = "rejected"
			}
		}
		if len(seenRows) > 0 && err == nil {
			err = fmt.Errorf("choose a source row awaiting link review")
		}
	default:
		err = fmt.Errorf("unknown workspace action")
	}
	if err == nil {
		raw, _ = json.Marshal(rows)
		linkRaw, _ = json.Marshal(links)
		_, err = tx.Exec(r.Context(), "UPDATE campaigns SET source_rows=$2,certificate_links=$3 WHERE id=$1", id, raw, linkRaw)
	}
	if err == nil && string(mappingRaw) != "{}" {
		if kind == "certificate" {
			var mapping Mapping
			err = json.Unmarshal(mappingRaw, &mapping)
			if err == nil {
				err = a.mapCampaignWith(r.Context(), tx, id, mapping)
			}
		} else {
			var mapping BulkMapping
			err = json.Unmarshal(mappingRaw, &mapping)
			if err == nil {
				err = a.mapBulkCampaignWith(r.Context(), tx, id, mapping.Template, mapping.Email, mapping.Columns, mapping.Constants)
			}
		}
	}
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		problem(w, 500, "changes could not be saved")
		return
	}
	a.audit(r.Context(), person(r).ID, "campaign.workspace."+in.Action, app, id)
	respond(w, 200, map[string]any{"rows": rows, "links": links, "revision": workspaceRevision(rows, links)})
}
func workspaceRevision(rows [][]string, links certificateLinks) string {
	b, _ := json.Marshal(links)
	return tokenHash(sourceRevision(rows) + string(b))
}

func workspaceDataRevision(data map[string]any) string {
	var rows [][]string
	raw, _ := json.Marshal(data["source_rows"])
	json.Unmarshal(raw, &rows)
	raw, _ = json.Marshal(data["certificate_links"])
	links, _ := decodeLinks(raw)
	return workspaceRevision(rows, links)
}
