package edp

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func testXLSX(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for name, value := range entries {
		part, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(part, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func xlsxFixture(sheet string) map[string]string {
	return map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="First tab" r:id="r1"/><sheet name="Second tab" r:id="r2"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="r1" Target="worksheets/sheet1.xml" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"/><Relationship Id="r2" Target="/xl/worksheets/sheet2.xml" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   sheet,
		"xl/worksheets/sheet2.xml":   `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>email</t></is></c></row><row r="2"><c r="A2" t="inlineStr"><is><t>second@example.test</t></is></c></row></sheetData></worksheet>`,
	}
}
func TestXLSXValuesAndHyperlinks(t *testing.T) {
	entries := xlsxFixture(`<worksheet xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheetData>
 <row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="inlineStr"><is><t>certificate_url</t></is></c><c r="C1" t="inlineStr"><is><t>score</t></is></c><c r="D1" t="inlineStr"><is><t>enabled</t></is></c></row>
 <row r="2"><c r="A2" t="s"><v>1</v></c><c r="B2" t="inlineStr"><is><t>Download certificate</t></is></c><c r="C2"><f>2+2</f><v>4</v></c><c r="D2" t="b"><v>1</v></c></row>
 <row r="4"><c r="A4" t="inlineStr"><is><r><t>foot</t></r><r><t>er</t></r></is></c></row>
 <row r="5"/>
 </sheetData><hyperlinks><hyperlink ref="B2" r:id="link1"/><hyperlink ref="A2" r:id="mail"/></hyperlinks></worksheet>`)
	entries["xl/sharedStrings.xml"] = `<sst><si><t>email</t></si><si><r><t>person</t></r><r><t>@example.test</t></r></si></sst>`
	entries["xl/worksheets/_rels/sheet1.xml.rels"] = `<Relationships><Relationship Id="link1" Target="https://example.test/certificate?id=1&amp;view=1" TargetMode="External" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"/><Relationship Id="mail" Target="mailto:person@example.test" TargetMode="External" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"/></Relationships>`
	data := testXLSX(t, entries)
	names, err := xlsxSheets(bytes.NewReader(data))
	if err != nil || !reflect.DeepEqual(names, []string{"First tab", "Second tab"}) {
		t.Fatalf("tabs %v: %v", names, err)
	}
	rows, err := parseImport(bytes.NewReader(data), "certificates.XLSX", "First tab")
	want := [][]string{{"email", "certificate_url", "score", "enabled"}, {"person@example.test", "https://example.test/certificate?id=1&view=1", "4", "TRUE"}, nil, {"footer"}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v: %v", rows, err)
	}
	second, err := parseXLSX(bytes.NewReader(data), "Second tab")
	if err != nil || second[1][0] != "second@example.test" {
		t.Fatalf("second tab: %v %v", second, err)
	}
	if _, err = parseXLSX(bytes.NewReader(data), "Missing"); err == nil {
		t.Fatal("missing tab accepted")
	}
}
func TestXLSXRejectsUnsafeAndMalformedInputs(t *testing.T) {
	normal := `<worksheet><sheetData><row r="1"><c t="inlineStr"><is><t>email</t></is></c></row><row r="2"><c t="inlineStr"><is><t>person@example.test</t></is></c></row></sheetData></worksheet>`
	cases := []struct {
		name   string
		change func(map[string]string)
	}{
		{"archive traversal", func(e map[string]string) { e["../escape.xml"] = "x" }},
		{"absolute archive", func(e map[string]string) { e["/absolute.xml"] = "x" }},
		{"backslash archive", func(e map[string]string) { e[`xl\escape.xml`] = "x" }},
		{"relationship traversal", func(e map[string]string) {
			e["xl/_rels/workbook.xml.rels"] = strings.ReplaceAll(e["xl/_rels/workbook.xml.rels"], "worksheets/sheet1.xml", "../../escape.xml")
		}},
		{"external worksheet", func(e map[string]string) {
			e["xl/_rels/workbook.xml.rels"] = strings.ReplaceAll(e["xl/_rels/workbook.xml.rels"], `Id="r1"`, `Id="r1" TargetMode="External"`)
		}},
		{"no data", func(e map[string]string) {
			e["xl/worksheets/sheet1.xml"] = `<worksheet><sheetData><row r="1"><c t="inlineStr"><is><t>email</t></is></c></row></sheetData></worksheet>`
		}},
		{"missing sheet", func(e map[string]string) { delete(e, "xl/worksheets/sheet1.xml") }},
		{"oversized column", func(e map[string]string) {
			e["xl/worksheets/sheet1.xml"] = strings.ReplaceAll(normal, `<c t=`, `<c r="BA1" t=`)
		}},
		{"oversized row", func(e map[string]string) {
			e["xl/worksheets/sheet1.xml"] = strings.ReplaceAll(normal, `r="2"`, `r="5002"`)
		}},
		{"wrong cell row", func(e map[string]string) {
			e["xl/worksheets/sheet1.xml"] = strings.ReplaceAll(normal, `<c t=`, `<c r="A9" t=`)
		}},
		{"invalid shared string", func(e map[string]string) {
			e["xl/worksheets/sheet1.xml"] = strings.Replace(normal, `<c t="inlineStr"><is><t>person@example.test</t></is></c>`, `<c t="s"><v>99</v></c>`, 1)
		}},
		{"unsafe hyperlink", func(e map[string]string) {
			e["xl/worksheets/sheet1.xml"] = strings.Replace(normal, `</worksheet>`, `<hyperlinks xmlns:r="urn:rel"><hyperlink ref="A2" r:id="link"/></hyperlinks></worksheet>`, 1)
			e["xl/worksheets/_rels/sheet1.xml.rels"] = `<Relationships><Relationship Id="link" Target="javascript:alert(1)" TargetMode="External" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"/></Relationships>`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := xlsxFixture(normal)
			tc.change(entries)
			if _, err := parseXLSX(bytes.NewReader(testXLSX(t, entries)), ""); err == nil {
				t.Fatal("unsafe/malformed workbook accepted")
			}
		})
	}
	if _, err := parseXLSX(strings.NewReader("not a zip"), ""); err == nil {
		t.Fatal("nonzip accepted")
	}
	if _, err := parseXLSX(strings.NewReader(strings.Repeat("x", importFileLimit+1)), ""); err == nil {
		t.Fatal("oversized upload accepted")
	}
}
func TestXLSXExpandedSizeBound(t *testing.T) {
	entries := xlsxFixture(`<worksheet/>`)
	entries["xl/large.xml"] = strings.Repeat("x", xlsxPartLimit+1)
	if _, err := xlsxSheets(bytes.NewReader(testXLSX(t, entries))); err == nil {
		t.Fatal("compressed oversized XML accepted")
	}
	entries = xlsxFixture(`<worksheet/>`)
	for i := 0; i < 3; i++ {
		entries[fmt.Sprintf("large%d.xml", i)] = strings.Repeat("x", 24<<20)
	}
	if _, err := xlsxSheets(bytes.NewReader(testXLSX(t, entries))); err == nil {
		t.Fatal("aggregate expansion limit bypassed")
	}
}
func TestImportNormalizationPreservesOrdinals(t *testing.T) {
	original := [][]string{{"email"}, {"a@example.test"}, {""}, {"footer"}, nil}
	rows, err := normalizeImportRows(original)
	if err != nil || !reflect.DeepEqual(rows, original[:4]) {
		t.Fatalf("normalization %v: %v", rows, err)
	}
	csv, err := parseImport(strings.NewReader("email,url\na@example.test,https://example.test\n"), "file.csv", "")
	if err != nil || len(csv) != 2 {
		t.Fatalf("CSV %v %v", csv, err)
	}
	if _, err := parseImport(strings.NewReader("ignored"), "file.xlsm", ""); err == nil {
		t.Fatal("macro extension accepted")
	}
}
