package edp

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Source exports describe the normalized import, including rows omitted from
// dispatch. The original workbook bytes are not retained by the importer.
func (a *App) campaignSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := a.one(r.Context(), "SELECT application_id,source_rows FROM campaigns WHERE id=$1", id)
	if err != nil {
		problem(w, 404, "campaign not found")
		return
	}
	if !a.require(w, r, data["application_id"].(string), "campaign.view") {
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "csv" && format != "json" {
		problem(w, 400, "source format must be csv or json")
		return
	}
	var source [][]string
	raw, err := json.Marshal(data["source_rows"])
	if err != nil || json.Unmarshal(raw, &source) != nil {
		problem(w, 500, "source snapshot unavailable")
		return
	}
	if len(source) == 0 {
		problem(w, 404, "campaign has no imported source snapshot")
		return
	}
	if format == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="campaign-source.json"`)
		respond(w, 200, map[string]any{"rows": source})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="campaign-source.csv"`)
	w.Write([]byte{0xef, 0xbb, 0xbf})
	out := csv.NewWriter(w)
	for _, row := range source {
		record := make([]string, len(row))
		for i, value := range row {
			record[i] = csvSafe(value)
		}
		if err := out.Write(record); err != nil {
			return
		}
	}
	out.Flush()
}

type campaignMappingField struct {
	Name   string
	Source string
}

// Use only saved mappings. Guessed import columns and current template versions
// are useful for draft editing but are not evidence of completed configuration.
func campaignMappingReview(kind string, value any, headers []string) ([]campaignMappingField, string) {
	raw, err := json.Marshal(value)
	if err != nil || string(raw) == "{}" || string(raw) == "null" {
		return nil, ""
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, ""
	}
	fields := []campaignMappingField{}
	column := func(name string, index int) {
		source := "No source column (default or constant)"
		if index >= 0 {
			source = fmt.Sprintf("Column %d", index+1)
			if index < len(headers) {
				source += " · " + headers[index]
			}
		}
		fields = append(fields, campaignMappingField{name, source})
	}
	var columns map[string]int
	var constants map[string]string
	if kind == "bulk" {
		var mapping BulkMapping
		if json.Unmarshal(raw, &mapping) != nil {
			return nil, ""
		}
		column("Recipient email", mapping.Email)
		fields = append(fields, campaignMappingField{"Template", mapping.Template})
		columns, constants = mapping.Columns, mapping.Constants
	} else {
		var mapping Mapping
		if json.Unmarshal(raw, &mapping) != nil {
			return nil, ""
		}
		column("Recipient email", mapping.Email)
		column("School", mapping.School)
		column("Certificate URL", mapping.URL)
		column("Event", mapping.Event)
		column("Award", mapping.Award)
		for _, index := range mapping.NameColumns {
			column("Participant name", index)
		}
		fields = append(fields, campaignMappingField{"Default event", mapping.DefaultEvent}, campaignMappingField{"Default award", mapping.DefaultAward}, campaignMappingField{"Skip rows without email", fmt.Sprint(mapping.SkipRowsWithoutEmail)}, campaignMappingField{"Combine certificates", fmt.Sprint(mapping.CombineRecipients)})
		if mapping.CombineRecipients {
			fields = append(fields, campaignMappingField{"Combined template", mapping.CombinedTemplate})
		}
		columns, constants = mapping.Columns, mapping.Constants
	}
	names := make([]string, 0, len(columns))
	for name := range columns {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		column("Variable: "+name, columns[name])
	}
	names = names[:0]
	for name := range constants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fields = append(fields, campaignMappingField{"Constant: " + name, constants[name]})
	}
	return fields, string(pretty)
}
