package edp

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const certificatePDFLimit = 20 << 20
const certificateTextLimit = 128 << 10
const certificatePageLimit = 20

type pdfExtraction struct {
	Text, Method string
	Pages        int
	Quality      int
}
type boundedCommandOutput struct {
	bytes.Buffer
	Limit int
}

func (b *boundedCommandOutput) Write(p []byte) (int, error) {
	if len(p) > b.Limit-b.Len() {
		return 0, fmt.Errorf("command output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func localPDFCommand(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	if runtime.GOOS == "linux" {
		// Positional arguments are passed verbatim, without shell interpolation.
		// A malformed PDF can fail this child process without exhausting EDP.
		limits := append([]string{"-c", `ulimit -v 524288; ulimit -t 60; exec "$@"`, "edp-pdf", name}, args...)
		command = exec.CommandContext(ctx, "sh", limits...)
	}
	command.Env = append(os.Environ(), "OMP_THREAD_LIMIT=1", "LC_ALL=C")
	var output boundedCommandOutput
	output.Limit = limit
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("local PDF extraction timed out or was cancelled")
		}
		return nil, fmt.Errorf("%s could not process this PDF", name)
	}
	return output.Bytes(), nil
}
func extractionCapabilities(ctx context.Context) error {
	for _, name := range []string{"pdfinfo", "pdftotext", "pdftoppm", "tesseract"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("local PDF tools are unavailable: install Poppler and Tesseract with English, Thai and orientation data")
		}
	}
	output, err := localPDFCommand(ctx, 32<<10, "tesseract", "--list-langs")
	if err != nil {
		return err
	}
	langs := map[string]bool{}
	for _, line := range strings.Fields(string(output)) {
		langs[line] = true
	}
	for _, lang := range []string{"eng", "tha", "osd"} {
		if !langs[lang] {
			return fmt.Errorf("Tesseract %s language data is missing", lang)
		}
	}
	return nil
}
func parsePDFPages(info string) (int, error) {
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "Pages:") {
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			if err == nil && n > 0 && n <= certificatePageLimit {
				return n, nil
			}
			return 0, fmt.Errorf("certificate PDF must contain 1–20 pages")
		}
	}
	return 0, fmt.Errorf("cannot read PDF page count")
}
func readablePDFText(text string) bool {
	count := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			count++
		}
	}
	return count >= 40 && !strings.ContainsRune(text, '\ufffd')
}
func ocrTSV(raw []byte) (string, int, error) {
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	var text strings.Builder
	weighted, total := 0, 0
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, fmt.Errorf("cannot read local OCR output")
		}
		if len(row) < 12 || row[0] != "5" {
			continue
		}
		value := strings.TrimSpace(strings.Join(row[11:], " "))
		confidence, err := strconv.ParseFloat(row[10], 64)
		if err != nil || value == "" || confidence < 0 {
			continue
		}
		if text.Len()+len(value)+1 > certificateTextLimit {
			return "", 0, fmt.Errorf("OCR text exceeds the 128 KiB limit")
		}
		text.WriteString(value)
		text.WriteByte(' ')
		weight := len([]rune(value))
		weighted += int(confidence) * weight
		total += weight
	}
	quality := 0
	if total > 0 {
		quality = weighted / total
	}
	return strings.TrimSpace(text.String()), quality, nil
}

// Every page is inspected: native text is retained and image-only/garbled pages
// are OCRed locally. No PDF bytes or extracted text leave the EDP server.
func extractCertificatePDF(ctx context.Context, data []byte, forceOCR ...bool) (pdfExtraction, error) {
	var result pdfExtraction
	if len(data) > certificatePDFLimit || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return result, fmt.Errorf("choose a valid PDF up to 20 MiB")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := extractionCapabilities(ctx); err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp("", "edp-certificate-")
	if err != nil {
		return result, fmt.Errorf("cannot create local PDF workspace")
	}
	defer os.RemoveAll(dir)
	pdf := filepath.Join(dir, "certificate.pdf")
	if err = os.WriteFile(pdf, data, 0600); err != nil {
		return result, err
	}
	info, err := localPDFCommand(ctx, 32<<10, "pdfinfo", pdf)
	if err != nil {
		return result, fmt.Errorf("PDF is unreadable, encrypted, or damaged")
	}
	result.Pages, err = parsePDFPages(string(info))
	if err != nil {
		return result, err
	}
	if strings.Contains(string(info), "Encrypted:       yes") {
		return result, fmt.Errorf("encrypted PDFs need manual review")
	}
	native, err := localPDFCommand(ctx, certificateTextLimit, "pdftotext", "-layout", "-enc", "UTF-8", pdf, "-")
	if err != nil {
		return result, err
	}
	pages := strings.Split(string(native), "\f")
	parts := []string{}
	ocrPages := 0
	quality := 100
	for page := 1; page <= result.Pages; page++ {
		text := ""
		if page-1 < len(pages) {
			text = pages[page-1]
		}
		if !readablePDFText(text) || (len(forceOCR) > 0 && forceOCR[0]) {
			prefix := filepath.Join(dir, "page")
			if _, err = localPDFCommand(ctx, 1024, "pdftoppm", "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-singlefile", "-r", "200", "-scale-to", "2500", "-gray", "-png", pdf, prefix); err != nil {
				return result, err
			}
			output, e := localPDFCommand(ctx, 4<<20, "tesseract", prefix+".png", "stdout", "-l", "eng+tha", "--psm", "1", "tsv")
			if e != nil {
				return result, e
			}
			var confidence int
			text, confidence, err = ocrTSV(output)
			if err != nil {
				return result, err
			}
			if strings.TrimSpace(text) == "" {
				// Sparse certificates can give orientation detection too little
				// text. Retry ordinary and sparse-page segmentation locally.
				for _, mode := range []string{"3", "11"} {
					retry, e := localPDFCommand(ctx, 4<<20, "tesseract", prefix+".png", "stdout", "-l", "eng+tha", "--psm", mode, "tsv")
					if e != nil {
						continue
					}
					value, q, e := ocrTSV(retry)
					if e == nil && strings.TrimSpace(value) != "" {
						text, confidence = value, q
						break
					}
				}
			}
			os.Remove(prefix + ".png")
			quality = min(quality, confidence)
			ocrPages++
		}
		parts = append(parts, strings.TrimSpace(text))
	}
	result.Text = strings.Join(parts, "\n\n")
	if len(result.Text) > certificateTextLimit {
		return result, fmt.Errorf("extracted text exceeds the 128 KiB limit")
	}
	if strings.TrimSpace(result.Text) == "" {
		return result, fmt.Errorf("no readable text found; review this PDF manually")
	}
	result.Quality = quality
	result.Method = "text"
	if ocrPages == result.Pages {
		result.Method = "ocr"
	} else if ocrPages > 0 {
		result.Method = "text+ocr"
	}
	return result, nil
}
