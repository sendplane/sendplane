package ingest

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNoEmailColumn is returned by CSVToNDJSON when the header row has no email
// column.
var ErrNoEmailColumn = errors.New("ingest: csv has no email column")

// ColumnMapping names the CSV columns that map onto RecipientLine fields.
// A zero field falls back to the canonical header name. Matching is
// case-insensitive and ignores surrounding space. Every other column becomes a
// string entry in vars, under its header name.
type ColumnMapping struct {
	Email          string // default "email"
	Name           string // default "name"
	Locale         string // default "locale"
	UnsubscribeURL string // default "unsubscribe_url"
}

func (m ColumnMapping) withDefaults() ColumnMapping {
	if m.Email == "" {
		m.Email = "email"
	}
	if m.Name == "" {
		m.Name = "name"
	}
	if m.Locale == "" {
		m.Locale = "locale"
	}
	if m.UnsubscribeURL == "" {
		m.UnsubscribeURL = "unsubscribe_url"
	}
	return m
}

// CSVToNDJSON converts a CSV upload into the NDJSON body Ingest reads. It is
// the UI's upload path (ADR-0005 rejected server-side file storage: the client
// converts and streams to the same endpoint), and it is offered here so a host
// that accepts CSV on its own side converts it exactly the way the console
// does.
//
// It streams: one record in, one line out, so a large file never lands in
// memory. Values are written as strings; CSVToNDJSON does not guess numbers or
// booleans, and empty cells are omitted rather than written as "". Rows are not
// validated beyond the CSV grammar, so a row with a broken address becomes a
// line that Ingest counts as invalid and reports with its line number.
func CSVToNDJSON(r io.Reader, w io.Writer, mapping ColumnMapping) error {
	m := mapping.withDefaults()
	cr := csv.NewReader(r)
	cr.ReuseRecord = true

	header, err := cr.Read()
	if err == io.EOF {
		return nil // empty file: nothing to convert
	}
	if err != nil {
		return fmt.Errorf("ingest: csv header: %w", err)
	}

	const (
		colVars = iota
		colEmail
		colName
		colLocale
		colUnsub
	)
	kind := make([]int, len(header))
	names := make([]string, len(header))
	emailCol := -1
	for i, h := range header {
		h = strings.TrimSpace(h)
		names[i] = h
		switch strings.ToLower(h) {
		case strings.ToLower(m.Email):
			kind[i], emailCol = colEmail, i
		case strings.ToLower(m.Name):
			kind[i] = colName
		case strings.ToLower(m.Locale):
			kind[i] = colLocale
		case strings.ToLower(m.UnsubscribeURL):
			kind[i] = colUnsub
		default:
			kind[i] = colVars
		}
	}
	if emailCol < 0 {
		return fmt.Errorf("%w: looked for %q in %v", ErrNoEmailColumn, m.Email, header)
	}

	enc := json.NewEncoder(w)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ingest: csv record: %w", err)
		}
		var line RecipientLine
		for i, v := range rec {
			if i >= len(kind) {
				break
			}
			v = strings.TrimSpace(v)
			switch kind[i] {
			case colEmail:
				line.Email = v
			case colName:
				line.Name = v
			case colLocale:
				line.Locale = v
			case colUnsub:
				line.UnsubscribeURL = v
			default:
				if v == "" || names[i] == "" {
					continue
				}
				if line.Vars == nil {
					line.Vars = make(map[string]any, 4)
				}
				line.Vars[names[i]] = v
			}
		}
		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("ingest: write ndjson: %w", err)
		}
	}
}
