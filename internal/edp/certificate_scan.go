package edp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (a *App) certificateDriveClient(ctx context.Context) (*http.Client, error) {
	return a.googleReadClient(ctx, "", "")
}
func downloadCertificatePDF(ctx context.Context, client *http.Client, id string) ([]byte, error) {
	if !googleFileID.MatchString(id) {
		return nil, fmt.Errorf("invalid PDF file ID")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/drive/v3/files/"+url.PathEscape(id)+"?alt=media&supportsAllDrives=true", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Google Drive PDF download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("Google Drive PDF returned %d; check file read/download permission", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, certificatePDFLimit+1))
	if err != nil {
		return nil, fmt.Errorf("PDF download was interrupted")
	}
	if len(data) > certificatePDFLimit {
		return nil, fmt.Errorf("PDF exceeds the 20 MiB limit")
	}
	return data, nil
}
func queueCertificateScan(ctx context.Context, tx pgx.Tx, id string, links *certificateLinks) error {
	if _, err := tx.Exec(ctx, "DELETE FROM certificate_scan_files WHERE campaign_id=$1", id); err != nil {
		return err
	}
	links.ScanID = newID("scan_")
	links.ScanState = "reading"
	batch := &pgx.Batch{}
	for _, f := range links.Files {
		batch.Queue("INSERT INTO certificate_scan_files(id,campaign_id,scan_id,file_id,filename) VALUES($1,$2,$3,$4,$5)", newID("pdf_"), id, links.ScanID, f.ID, f.Name)
	}
	results := tx.SendBatch(ctx, batch)
	defer results.Close()
	for range links.Files {
		if _, err := results.Exec(); err != nil {
			return err
		}
	}
	return results.Close()
}

type certificateScanJob struct {
	ID, Campaign, Scan, File, Lease string
	ForceOCR                        bool
	GoogleUser, GoogleConnection    string
}

func (a *App) claimCertificateScan(ctx context.Context) (*certificateScanJob, error) {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(824199)"); err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, "UPDATE certificate_scan_files SET state='error',error='Local scan interrupted repeatedly; rescan this folder',finished_at=now() WHERE state='processing' AND lease_until<now() AND attempts>=3")
	if err != nil {
		return nil, err
	}
	var active int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM certificate_scan_files WHERE state='processing' AND lease_until>now()").Scan(&active); err != nil || active >= 2 {
		return nil, err
	}
	j := &certificateScanJob{}
	err = tx.QueryRow(ctx, `SELECT f.id,f.campaign_id,f.scan_id,f.file_id,coalesce((c.certificate_links->>'force_ocr')::boolean,false),coalesce(c.certificate_links->>'google_user',''),coalesce(c.certificate_links->>'google_connection','') FROM certificate_scan_files f JOIN campaigns c ON c.id=f.campaign_id WHERE c.state='draft' AND c.certificate_links->>'scan_id'=f.scan_id AND (f.state='queued' OR (f.state='processing' AND f.lease_until<now() AND f.attempts<3)) ORDER BY f.created_at,f.id FOR UPDATE OF f SKIP LOCKED LIMIT 1`).Scan(&j.ID, &j.Campaign, &j.Scan, &j.File, &j.ForceOCR, &j.GoogleUser, &j.GoogleConnection)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Lease = newID("pdflease_")
	_, err = tx.Exec(ctx, "UPDATE certificate_scan_files SET state='processing',lease_token=$2,lease_until=now()+interval '5 minutes',attempts=attempts+1 WHERE id=$1", j.ID, j.Lease)
	if err != nil {
		return nil, err
	}
	return j, tx.Commit(ctx)
}
func (a *App) processCertificateScan(ctx context.Context, j certificateScanJob) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	var result pdfExtraction
	var checksum string
	client, err := a.googleReadClient(ctx, j.GoogleUser, j.GoogleConnection)
	if err == nil {
		var data []byte
		data, err = downloadCertificatePDF(ctx, client, j.File)
		if err == nil {
			hash := sha256.Sum256(data)
			checksum = hex.EncodeToString(hash[:])
			result, err = extractCertificatePDF(ctx, data, j.ForceOCR)
		}
	}
	if ctx.Err() != nil {
		return
	} // Let the bounded lease resume after interruption.
	state, message := "done", ""
	encrypted := ""
	if err != nil {
		state, message = "error", err.Error()
	} else {
		encrypted = seal(a.C.Secret, result.Text)
	}
	a.DB.Exec(ctx, "UPDATE certificate_scan_files SET state=$3,text_enc=$4,pages=$5,method=$6,quality=$7,checksum=$8,error=$9,finished_at=now(),lease_until=NULL WHERE id=$1 AND lease_token=$2 AND state='processing'", j.ID, j.Lease, state, encrypted, result.Pages, result.Method, result.Quality, checksum, message)
}
func (a *App) certificateScanLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		a.finalizeCertificateScans(ctx)
		job, err := a.claimCertificateScan(ctx)
		if err == nil && job != nil {
			a.processCertificateScan(ctx, *job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *App) finalizeCertificateScans(ctx context.Context) {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var id, scope, kind string
	var raw, sourceRaw, mappingRaw []byte
	err = tx.QueryRow(ctx, `SELECT c.id,c.application_id,c.kind,c.certificate_links,c.source_rows,c.mapping FROM campaigns c WHERE c.state='draft' AND c.certificate_links->>'scan_state'='reading' AND NOT EXISTS(SELECT 1 FROM certificate_scan_files f WHERE f.campaign_id=c.id AND f.scan_id=c.certificate_links->>'scan_id' AND f.state IN('queued','processing')) ORDER BY c.created_at FOR UPDATE OF c SKIP LOCKED LIMIT 1`).Scan(&id, &scope, &kind, &raw, &sourceRaw, &mappingRaw)
	if err != nil {
		return
	}
	links, err := decodeLinks(raw)
	if err != nil {
		return
	}
	var source [][]string
	if json.Unmarshal(sourceRaw, &source) != nil {
		return
	}
	jobs, err := tx.Query(ctx, "SELECT file_id,text_enc,method,pages,quality,error FROM certificate_scan_files WHERE campaign_id=$1 AND scan_id=$2", id, links.ScanID)
	if err != nil {
		return
	}
	extracted := map[string]drivePDF{}
	used := 0
	for jobs.Next() {
		var id, encrypted, method, message string
		var pages, quality int
		if jobs.Scan(&id, &encrypted, &method, &pages, &quality, &message) != nil {
			jobs.Close()
			return
		}
		text, e := unseal(a.C.Secret, encrypted)
		if e != nil {
			message = "Extracted text could not be decrypted"
		}
		if used+len(text) > 64<<20 {
			text = ""
			message = "Folder text exceeds 64 MiB; use a smaller folder"
		}
		used += len(text)
		extracted[id] = drivePDF{ID: id, Text: text, Method: method, Pages: pages, Quality: quality, Error: message, ContentRead: true}
	}
	err = jobs.Err()
	jobs.Close()
	if err != nil {
		return
	}
	for i := range links.Files {
		if read, ok := extracted[links.Files[i].ID]; ok {
			read.Name = links.Files[i].Name
			links.Files[i] = read
		} else {
			links.Files[i].ContentRead = true
			links.Files[i].Error = "PDF could not be read"
		}
	}
	links.Matches = matchCertificateContents(source, links.Files, links.Columns, links.URLColumn)
	links.ScanState = "complete"
	if links.AutoThreshold > 0 {
		applyAutoCertificateLinks(source, &links, links.AutoThreshold)
	}
	raw, _ = json.Marshal(links)
	sourceRaw, _ = json.Marshal(source)
	if _, err = tx.Exec(ctx, "UPDATE campaigns SET certificate_links=$2,source_rows=$3 WHERE id=$1", id, raw, sourceRaw); err != nil {
		return
	}
	if string(mappingRaw) != "{}" && kind == "certificate" {
		var mapping Mapping
		if json.Unmarshal(mappingRaw, &mapping) != nil {
			return
		}
		if a.mapCampaignWith(ctx, tx, id, mapping) != nil {
			return
		}
	}
	if tx.Commit(ctx) == nil {
		a.audit(ctx, links.Actor, "campaign.certificates.scanned", scope, id)
	}
}
func applyAutoCertificateLinks(rows [][]string, links *certificateLinks, threshold int) {
	known := map[string]drivePDF{}
	for _, f := range links.Files {
		known[f.ID] = f
	}
	used := map[string]bool{}
	for _, row := range rows[1:] {
		if link := cell(row, links.URLColumn); link != "" {
			used[link] = true
		}
	}
	for i := range links.Matches {
		m := &links.Matches[i]
		f, ok := known[m.FileID]
		if !ok || m.Status != "pending" || m.Ambiguous || m.Score < threshold || used[f.link()] {
			continue
		}
		for len(rows[m.Row-1]) <= links.URLColumn {
			rows[m.Row-1] = append(rows[m.Row-1], "")
		}
		rows[m.Row-1][links.URLColumn] = f.link()
		m.Status = "accepted"
		m.ReviewedBy = links.Actor
		now := time.Now().UTC()
		m.ReviewedAt = &now
		used[f.link()] = true
	}
}
func (a *App) certificateScanProgress(ctx context.Context, id, scan string) map[string]any {
	var total, done, failed, active int
	a.DB.QueryRow(ctx, "SELECT count(*),count(*) FILTER(WHERE state='done'),count(*) FILTER(WHERE state='error'),count(*) FILTER(WHERE state='processing') FROM certificate_scan_files WHERE campaign_id=$1 AND scan_id=$2", id, scan).Scan(&total, &done, &failed, &active)
	return map[string]any{"total": total, "done": done, "failed": failed, "active": active}
}
func (a *App) certificateExtractedText(w http.ResponseWriter, r *http.Request) {
	campaign, err := a.one(r.Context(), "SELECT application_id,certificate_links->>'scan_id' AS scan_id FROM campaigns WHERE id=$1", r.PathValue("id"))
	if err != nil {
		problem(w, 404, "campaign not found")
		return
	}
	if !a.require(w, r, campaign["application_id"].(string), "campaign.view") {
		return
	}
	var encrypted, method, message string
	var pages, quality int
	err = a.DB.QueryRow(r.Context(), "SELECT text_enc,method,pages,quality,error FROM certificate_scan_files WHERE campaign_id=$1 AND scan_id=$2 AND file_id=$3 AND state IN('done','error')", r.PathValue("id"), campaign["scan_id"], r.PathValue("file")).Scan(&encrypted, &method, &pages, &quality, &message)
	if err != nil {
		problem(w, 404, "extracted text is unavailable or still processing")
		return
	}
	if message != "" {
		problem(w, 400, message)
		return
	}
	text, err := unseal(a.C.Secret, encrypted)
	if err != nil {
		problem(w, 500, "extracted text is unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"text": text, "method": method, "pages": pages, "ocr_quality": quality})
}

// Thai OCR sometimes inserts spaces inside names. Compact matching is limited
// to Thai identity values; Latin identifiers retain word boundaries.
func documentContains(text, key string) bool {
	if strings.Contains(" "+text+" ", " "+key+" ") {
		return true
	}
	thai := false
	for _, r := range key {
		if r >= 0x0e00 && r <= 0x0e7f {
			thai = true
			break
		}
	}
	return thai && len([]rune(key)) >= 4 && strings.Contains(strings.ReplaceAll(text, " ", ""), strings.ReplaceAll(key, " ", ""))
}
func matchCertificateContents(rows [][]string, files []drivePDF, columns []int, urlColumn int) []certificateMatch {
	matches := matchCertificates(rows, files, columns, urlColumn)
	documents := make([]string, len(files))
	words := map[string][]int{}
	for i, f := range files {
		documents[i] = matchText(f.Text)
		seen := map[string]bool{}
		for _, word := range strings.Fields(documents[i]) {
			if !seen[word] {
				words[word] = append(words[word], i)
				seen[word] = true
			}
		}
	}
	for i := range matches {
		m := &matches[i]
		if m.Status == "existing" {
			continue
		}
		row := rows[m.Row-1]
		keys := []string{}
		for _, c := range columns {
			if value := matchText(cell(row, c)); len([]rune(value)) >= 3 {
				keys = append(keys, value)
			}
		}
		candidates := map[int]bool{}
		if len(files) <= 250 {
			for j := range files {
				candidates[j] = true
			}
		} else {
			for _, key := range keys {
				var shortlist []int
				for _, word := range strings.Fields(key) {
					if hits := words[word]; len(hits) > 0 && (shortlist == nil || len(hits) < len(shortlist)) {
						shortlist = hits
					}
				}
				if len(shortlist) <= 250 {
					for _, j := range shortlist {
						candidates[j] = true
					}
				}
			}
		}
		best, runner := 0, 0
		var winner *drivePDF
		for j := range files {
			if !candidates[j] {
				continue
			}
			f := &files[j]
			if f.Text == "" || f.Error != "" {
				continue
			}
			hits := 0
			for _, key := range keys {
				if documentContains(documents[j], key) {
					hits++
				}
			}
			score := 0
			if hits > 0 {
				score = 60 + 25*hits/max(1, len(columns))
				if hits == len(columns) {
					score = 98
					if len(columns) > 1 {
						score = 100
					}
					if strings.Contains(f.Method, "ocr") {
						score = 90
						if len(columns) > 1 {
							score = 95
						}
						if f.Quality < 75 {
							score = min(score, 84)
						}
					}
				}
			}
			if score > best {
				runner = best
				best = score
				winner = f
			} else if score > runner {
				runner = score
			}
		}
		if winner != nil {
			m.FileID = winner.ID
			m.Score = best
			m.Method = winner.Method
			m.Reason = fmt.Sprintf("%s evidence in PDF text (%d selected identity columns)", winner.Method, len(columns))
			matched := []string{}
			for _, key := range keys {
				if documentContains(matchText(winner.Text), key) {
					matched = append(matched, key)
				}
			}
			m.Evidence = strings.Join(matched, " / ")
			method := "PDF text"
			if strings.Contains(winner.Method, "ocr") {
				method = "local OCR"
			}
			m.Reason = fmt.Sprintf("%d of %d identity values confirmed by %s", len(matched), len(columns), method)
			if strings.Contains(winner.Method, "ocr") {
				m.Reason += fmt.Sprintf("; OCR quality %d%%", winner.Quality)
			}
			m.Ambiguous = best-runner < 10 || best < 90
		} else {
			m.Score = min(m.Score, 69)
			m.Ambiguous = true
			m.Method = "filename"
			m.Reason += "; PDF text did not confirm the identity, so manual review is required"
		}
	}
	uses := map[string]int{}
	for _, m := range matches {
		if m.FileID != "" {
			uses[m.FileID]++
		}
	}
	for i := range matches {
		if uses[matches[i].FileID] > 1 {
			matches[i].Ambiguous = true
			matches[i].Reason += "; this PDF is suggested for multiple rows"
		}
	}
	return matches
}
