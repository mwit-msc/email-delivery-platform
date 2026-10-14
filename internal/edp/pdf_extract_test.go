package edp

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticPDF(objects [][]byte) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	offset := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, position := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", position)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), offset)
	return out.Bytes()
}
func textCertificatePDF(identity ...string) []byte {
	text := "BT /F1 25 Tf 65 710 Td (Certificate of Participation) Tj 0 -55 Td (Alice Example) Tj 0 -45 Td (Certificate ID CERT-001) Tj 0 -45 Td (Example School) Tj 0 -45 Td (Participation Award 2026) Tj ET"
	if len(identity) > 0 {
		text = strings.ReplaceAll(text, "Alice Example", identity[0])
	}
	return syntheticPDF([][]byte{[]byte("<< /Type /Catalog /Pages 2 0 R >>"), []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"), []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"), []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"), []byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(text), text))})
}
func scannedCertificatePDF(t *testing.T, identity ...string) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "original.pdf")
	if err := os.WriteFile(path, textCertificatePDF(identity...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := localPDFCommand(context.Background(), 1024, "pdftoppm", "-singlefile", "-r", "150", "-png", path, filepath.Join(dir, "render")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "render.png"))
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	rgb := image.NewRGBA(img.Bounds())
	draw.Draw(rgb, rgb.Bounds(), img, img.Bounds().Min, draw.Src)
	var jpegData bytes.Buffer
	if err = jpeg.Encode(&jpegData, rgb, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	stream := "q 612 0 0 792 0 0 cm /Image Do Q"
	imageObject := append([]byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n", img.Bounds().Dx(), img.Bounds().Dy(), jpegData.Len())), jpegData.Bytes()...)
	imageObject = append(imageObject, []byte("\nendstream")...)
	return syntheticPDF([][]byte{[]byte("<< /Type /Catalog /Pages 2 0 R >>"), []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"), []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /Image 5 0 R >> >> /Contents 4 0 R >>"), []byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)), imageObject})
}
func TestLocalCertificateTextAndOCR(t *testing.T) {
	ctx := context.Background()
	if err := extractionCapabilities(ctx); err != nil {
		t.Skip(err)
	}
	text, err := extractCertificatePDF(ctx, textCertificatePDF())
	if err != nil || text.Method != "text" || !strings.Contains(text.Text, "Alice Example") || !strings.Contains(text.Text, "CERT-001") {
		t.Fatal("native text extraction", text, err)
	}
	scan, err := extractCertificatePDF(ctx, scannedCertificatePDF(t))
	if err != nil || scan.Method != "ocr" || !strings.Contains(scan.Text, "Alice Example") || scan.Quality < 60 {
		t.Fatal("local image OCR", scan, err)
	}
	forced, err := extractCertificatePDF(ctx, textCertificatePDF(), true)
	if err != nil || forced.Method != "ocr" || !strings.Contains(forced.Text, "Alice Example") {
		t.Fatal("force OCR", forced, err)
	}
	if _, err := extractCertificatePDF(ctx, []byte("not a PDF")); err == nil {
		t.Fatal("invalid PDF accepted")
	}
}
func TestPDFContentEvidenceBeatsOpaqueFilenames(t *testing.T) {
	rows := [][]string{{"name", "id", "link"}, {"Alice Example", "CERT-001", ""}, {"Bob Example", "CERT-002", ""}}
	files := []drivePDF{{ID: "opaque-one", Name: "001.pdf", Text: "Certificate of participation Alice Example Certificate ID CERT-001 Example School", Method: "text", Quality: 100}, {ID: "opaque-two", Name: "002.pdf", Text: "Bob Example CERT-002", Method: "ocr", Quality: 90}}
	matches := matchCertificateContents(rows, files, []int{0, 1}, 2)
	if matches[0].FileID != "opaque-one" || matches[0].Score != 100 || matches[0].Ambiguous || matches[1].Score != 95 || matches[1].Method != "ocr" {
		t.Fatal("PDF text did not identify opaque filenames", matches)
	}
	files[1].Quality = 30
	matches = matchCertificateContents(rows, files, []int{0, 1}, 2)
	if matches[1].Score >= 90 || !matches[1].Ambiguous {
		t.Fatal("poor OCR was auto eligible", matches[1])
	}
	files[0].Text = "This certificate belongs to somebody else"
	files[0].Name = "Alice Example CERT-001.pdf"
	matches = matchCertificateContents(rows, files, []int{0, 1}, 2)
	if matches[0].Score >= 70 || !matches[0].Ambiguous {
		t.Fatal("filename overruled contradictory PDF text", matches[0])
	}
	if !documentContains(matchText("เกียรติบัตร ณั ฐพล ทด สอบ"), matchText("ณัฐพล ทดสอบ")) {
		t.Fatal("Thai OCR spaces prevented matching")
	}
	if documentContains("certificate cert 0019", "cert 001") {
		t.Fatal("identifier substring was treated as exact")
	}
}
