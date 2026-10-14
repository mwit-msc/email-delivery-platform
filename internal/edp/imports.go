package edp

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"
)

const (
	importFileLimit   = 8 << 20
	xlsxExpandedLimit = 64 << 20
	xlsxPartLimit     = 32 << 20
)

type xlsxBook struct {
	files   map[string]*zip.File
	sheets  []xlsxSheet
	strings []string
}
type xlsxSheet struct{ Name, ID, File string }
type xlsxRelationship struct {
	ID     string `xml:"Id,attr"`
	Target string `xml:"Target,attr"`
	Mode   string `xml:"TargetMode,attr"`
	Type   string `xml:"Type,attr"`
}

// parseImport imports CSV or an explicitly selected XLSX tab. No external links
// or formulas are fetched or evaluated; only workbook values are imported.
func parseImport(r io.Reader, filename, sheet string) ([][]string, error) {
	switch strings.ToLower(path.Ext(filename)) {
	case ".xlsx":
		return parseXLSX(r, sheet)
	case ".csv", ".tsv":
		rows, err := parseCSV(r)
		if err != nil {
			return nil, err
		}
		return normalizeImportRows(rows)
	default:
		return nil, fmt.Errorf("upload a CSV, TSV or XLSX file")
	}
}

// normalizeImportRows trims only trailing blank rows. Interior rows and nonempty
// footer rows remain in place so mapping ordinals match the source worksheet.
func normalizeImportRows(rows [][]string) ([][]string, error) {
	last, nonemptyRows := -1, 0
	for i, row := range rows {
		if len(row) > 52 {
			return nil, fmt.Errorf("maximum 52 columns")
		}
		for _, value := range row {
			if strings.TrimSpace(value) != "" {
				last = i
				nonemptyRows++
				break
			}
		}
	}
	if last >= 5001 {
		return nil, fmt.Errorf("import must contain a header and 1–5000 data rows")
	}
	if nonemptyRows < 2 {
		return nil, fmt.Errorf("selected import must contain a header and 1–5000 data rows")
	}
	return rows[:last+1], nil
}

func openXLSX(r io.Reader) (*xlsxBook, error) {
	data, err := io.ReadAll(io.LimitReader(r, importFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > importFileLimit {
		return nil, fmt.Errorf("XLSX exceeds the 8 MiB import limit")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid XLSX archive")
	}
	if len(archive.File) > 1024 {
		return nil, fmt.Errorf("XLSX contains too many archive entries")
	}
	book := &xlsxBook{files: map[string]*zip.File{}}
	var expanded uint64
	for _, f := range archive.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, ":") {
			return nil, fmt.Errorf("unsafe XLSX archive path")
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if _, exists := book.files[name]; exists {
			return nil, fmt.Errorf("duplicate XLSX archive entry")
		}
		if f.UncompressedSize64 > xlsxPartLimit || expanded > xlsxExpandedLimit-f.UncompressedSize64 {
			return nil, fmt.Errorf("XLSX expanded content exceeds the import limit")
		}
		expanded += f.UncompressedSize64
		book.files[name] = f
	}
	var workbook struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err = book.decode("xl/workbook.xml", &workbook); err != nil {
		return nil, err
	}
	relations, err := book.relationships("xl/_rels/workbook.xml.rels")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, s := range workbook.Sheets {
		if s.Name == "" || seen[s.Name] {
			return nil, fmt.Errorf("invalid or duplicate XLSX tab name")
		}
		seen[s.Name] = true
		relation, found := relations[s.ID]
		if !found || relation.Mode == "External" || !strings.HasSuffix(relation.Type, "/worksheet") {
			return nil, fmt.Errorf("invalid XLSX worksheet relationship")
		}
		target, err := xlsxTarget("xl/workbook.xml", relation.Target)
		if err != nil {
			return nil, err
		}
		if book.files[target] == nil {
			return nil, fmt.Errorf("XLSX worksheet is missing")
		}
		book.sheets = append(book.sheets, xlsxSheet{Name: s.Name, ID: s.ID, File: target})
	}
	if len(book.sheets) == 0 {
		return nil, fmt.Errorf("XLSX workbook has no tabs")
	}
	return book, nil
}
func xlsxTarget(source, target string) (string, error) {
	if strings.ContainsAny(target, "\\?#:") {
		return "", fmt.Errorf("unsafe XLSX relationship target")
	}
	resolved := path.Clean(path.Join(path.Dir(source), target))
	if strings.HasPrefix(target, "/") {
		resolved = path.Clean(strings.TrimPrefix(target, "/"))
	}
	if resolved == ".." || strings.HasPrefix(resolved, "../") || resolved == "." {
		return "", fmt.Errorf("unsafe XLSX relationship target")
	}
	return resolved, nil
}
func (b *xlsxBook) part(name string) (io.ReadCloser, error) {
	f := b.files[name]
	if f == nil {
		return nil, fmt.Errorf("XLSX part %s is missing", name)
	}
	r, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("cannot read XLSX part")
	}
	return r, nil
}
func (b *xlsxBook) decode(name string, target any) error {
	r, err := b.part(name)
	if err != nil {
		return err
	}
	defer r.Close()
	if err = xml.NewDecoder(io.LimitReader(r, xlsxPartLimit+1)).Decode(target); err != nil {
		return fmt.Errorf("invalid XLSX XML in %s", name)
	}
	return nil
}
func (b *xlsxBook) relationships(name string) (map[string]xlsxRelationship, error) {
	var data struct {
		Items []xlsxRelationship `xml:"Relationship"`
	}
	if err := b.decode(name, &data); err != nil {
		return nil, err
	}
	out := map[string]xlsxRelationship{}
	for _, item := range data.Items {
		if item.ID == "" {
			return nil, fmt.Errorf("invalid XLSX relationship")
		}
		if _, duplicate := out[item.ID]; duplicate {
			return nil, fmt.Errorf("duplicate XLSX relationship")
		}
		out[item.ID] = item
	}
	return out, nil
}
func xlsxSheets(r io.Reader) ([]string, error) {
	b, err := openXLSX(r)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(b.sheets))
	for i, s := range b.sheets {
		names[i] = s.Name
	}
	return names, nil
}

type xlsxText struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (v xlsxText) value() string {
	var out strings.Builder
	out.WriteString(v.Text)
	for _, run := range v.Runs {
		out.WriteString(run.Text)
	}
	return out.String()
}
func (b *xlsxBook) sharedStrings() error {
	if b.files["xl/sharedStrings.xml"] == nil {
		return nil
	}
	r, err := b.part("xl/sharedStrings.xml")
	if err != nil {
		return err
	}
	defer r.Close()
	dec := xml.NewDecoder(io.LimitReader(r, xlsxPartLimit+1))
	for {
		token, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid XLSX shared strings")
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "si" {
			continue
		}
		var value xlsxText
		if err = dec.DecodeElement(&value, &start); err != nil {
			return fmt.Errorf("invalid XLSX shared string")
		}
		if len(b.strings) >= 260052 {
			return fmt.Errorf("too many XLSX shared strings")
		}
		b.strings = append(b.strings, value.value())
	}
}
func xlsxCellPosition(ref string) (int, int, error) {
	col, split := 0, 0
	for split < len(ref) && ref[split] >= 'A' && ref[split] <= 'Z' {
		col = col*26 + int(ref[split]-'A'+1)
		if col > 52 {
			return 0, 0, fmt.Errorf("maximum 52 columns")
		}
		split++
	}
	row, err := strconv.Atoi(ref[split:])
	if split == 0 || err != nil || row < 1 || row > 1048576 {
		return 0, 0, fmt.Errorf("invalid XLSX cell reference")
	}
	return col - 1, row, nil
}

type xlsxCell struct {
	Ref    string   `xml:"r,attr"`
	Type   string   `xml:"t,attr"`
	Value  string   `xml:"v"`
	Inline xlsxText `xml:"is"`
}

func xlsxReadRow(dec *xml.Decoder, start xml.StartElement) (int, []xlsxCell, error) {
	number := 0
	for _, attr := range start.Attr {
		if attr.Name.Local == "r" {
			n, err := strconv.Atoi(attr.Value)
			if err != nil || n < 1 {
				return 0, nil, fmt.Errorf("invalid XLSX row number")
			}
			number = n
		}
	}
	var cells []xlsxCell
	for {
		token, err := dec.Token()
		if err != nil {
			return 0, nil, fmt.Errorf("invalid XLSX row")
		}
		switch token := token.(type) {
		case xml.EndElement:
			if token.Name == start.Name {
				return number, cells, nil
			}
		case xml.StartElement:
			if token.Name.Local != "c" {
				if err := dec.Skip(); err != nil {
					return 0, nil, fmt.Errorf("invalid XLSX row")
				}
				continue
			}
			if len(cells) >= 52 {
				return 0, nil, fmt.Errorf("maximum 52 columns")
			}
			var cell xlsxCell
			if err := dec.DecodeElement(&cell, &token); err != nil {
				return 0, nil, fmt.Errorf("invalid XLSX cell")
			}
			cells = append(cells, cell)
		}
	}
}

func parseXLSX(r io.Reader, sheet string) ([][]string, error) {
	b, err := openXLSX(r)
	if err != nil {
		return nil, err
	}
	selected := b.sheets[0]
	if sheet != "" {
		found := false
		for _, s := range b.sheets {
			if s.Name == sheet {
				selected, found = s, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("requested XLSX tab does not exist")
		}
	}
	if err = b.sharedStrings(); err != nil {
		return nil, err
	}
	reader, err := b.part(selected.File)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	dec := xml.NewDecoder(io.LimitReader(reader, xlsxPartLimit+1))
	rows := make([][]string, 0)
	positions := map[int]int{}
	var hyperlinks []struct {
		Ref string `xml:"ref,attr"`
		ID  string `xml:"id,attr"`
	}
	lastRow := 0
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid XLSX worksheet")
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "row":
			number, cells, err := xlsxReadRow(dec, start)
			if err != nil {
				return nil, err
			}
			if number == 0 {
				number = lastRow + 1
			}
			if number <= lastRow || number > 1048576 {
				return nil, fmt.Errorf("invalid XLSX row order")
			}
			lastRow = number
			if len(cells) > 52 {
				return nil, fmt.Errorf("maximum 52 columns")
			}
			values := []string{}
			previous := -1
			for _, c := range cells {
				col := previous + 1
				if c.Ref != "" {
					var row int
					col, row, err = xlsxCellPosition(c.Ref)
					if err != nil {
						return nil, err
					}
					if row != number {
						return nil, fmt.Errorf("XLSX cell belongs to another row")
					}
				}
				if col <= previous || col >= 52 {
					return nil, fmt.Errorf("invalid XLSX cell order or column limit")
				}
				previous = col
				for len(values) <= col {
					values = append(values, "")
				}
				value := c.Value
				switch c.Type {
				case "s":
					index, e := strconv.Atoi(c.Value)
					if e != nil || index < 0 || index >= len(b.strings) {
						return nil, fmt.Errorf("invalid XLSX shared string index")
					}
					value = b.strings[index]
				case "inlineStr":
					value = c.Inline.value()
				case "b":
					if value == "1" {
						value = "TRUE"
					} else if value == "0" {
						value = "FALSE"
					} else {
						return nil, fmt.Errorf("invalid XLSX boolean")
					}
				}
				values[col] = value
			}
			if number > 5001 {
				return nil, fmt.Errorf("XLSX contains rows beyond the 5000 data-row import limit")
			}
			for len(rows) < number {
				rows = append(rows, nil)
			}
			positions[number] = number - 1
			rows[number-1] = values

		case "hyperlink":
			var link struct {
				Ref string `xml:"ref,attr"`
				ID  string `xml:"id,attr"`
			}
			if err = dec.DecodeElement(&link, &start); err != nil {
				return nil, fmt.Errorf("invalid XLSX hyperlink")
			}
			if len(hyperlinks) >= 260052 {
				return nil, fmt.Errorf("too many XLSX hyperlinks")
			}
			hyperlinks = append(hyperlinks, link)
		}
	}
	if len(hyperlinks) > 0 {
		relPath := path.Join(path.Dir(selected.File), "_rels", path.Base(selected.File)+".rels")
		relations := map[string]xlsxRelationship{}
		if b.files[relPath] != nil {
			relations, err = b.relationships(relPath)
			if err != nil {
				return nil, err
			}
		}
		for _, link := range hyperlinks {
			if link.ID == "" {
				continue
			} // Internal workbook locations are not recipient URLs.
			relation, exists := relations[link.ID]
			if !exists {
				return nil, fmt.Errorf("missing XLSX hyperlink relationship")
			}
			u, err := url.Parse(relation.Target)
			// Spreadsheet editors commonly hyperlink email addresses using mailto.
			// Retain the visible cell value; mailto is not a certificate target.
			if err == nil && u.Scheme == "mailto" && relation.Mode == "External" && strings.HasSuffix(relation.Type, "/hyperlink") {
				continue
			}
			if relation.Mode != "External" || !strings.HasSuffix(relation.Type, "/hyperlink") || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
				return nil, fmt.Errorf("XLSX hyperlinks must use HTTP or HTTPS URLs")
			}
			if strings.Contains(link.Ref, ":") {
				return nil, fmt.Errorf("XLSX hyperlink ranges are not supported; use individual cells")
			}
			col, row, err := xlsxCellPosition(link.Ref)
			if err != nil {
				return nil, err
			}
			index, found := positions[row]
			if !found {
				continue
			}
			for len(rows[index]) <= col {
				rows[index] = append(rows[index], "")
			}
			rows[index][col] = relation.Target
		}
	}
	return normalizeImportRows(rows)
}

// importHeader allows title and instruction rows above the chosen header.
func importHeader(rows [][]string, value string) ([][]string, error) {
	if value == "" {
		value = "1"
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n >= len(rows) {
		return nil, fmt.Errorf("choose a header row before the last data row")
	}
	return normalizeImportRows(rows[n-1:])
}
