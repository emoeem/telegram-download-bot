package archive

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Column reshaping for exports.
//
// The archive's export rows are just header + []string, which is what makes
// them cheap to stream. That shape is kept here: "which columns, in what order,
// under what name" is answered once, up front, and then applied per row. The
// alternative — a struct per view — would need one struct per export kind and
// would make a site-specific import schema a code change instead of a flag.
//
// Both front ends use this file: the web handler parses the same syntax from a
// query string, the CLI from flags.

// FieldSpec is a parsed "--fields" value: an ordered column list plus renames.
type FieldSpec struct {
	fields  []string
	renames map[string]string
}

// parseFieldSpec reads "a,b:out,c:d" into an ordered list plus a rename map.
//
// The colon form exists because dropping a column and renaming the rest is one
// intent ("make this fit the site's import schema"), and making users pass two
// separate flags for one intent is how flags get misused.
func ParseFieldSpec(spec string) (FieldSpec, error) {
	out := FieldSpec{renames: make(map[string]string)}
	for _, raw := range strings.Split(spec, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		src, dst, hasRename := strings.Cut(part, ":")
		src = strings.TrimSpace(src)
		if src == "" {
			return out, fmt.Errorf("empty field in %q", raw)
		}
		out.fields = append(out.fields, src)
		if hasRename {
			if dst = strings.TrimSpace(dst); dst != "" {
				out.renames[src] = dst
			}
		}
	}
	if len(out.fields) == 0 && len(out.renames) == 0 {
		return out, nil
	}
	return out, nil
}

// loadFieldMapping reads a {"source":"output"} JSON object. A mapping without a
// field list renames in place, which is the common case for a site importer
// that wants the archive's columns under its own names.
func LoadFieldMapping(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse mapping %s: %w", path, err)
	}
	return m, nil
}

// mergeRenames combines an inline spec with a mapping file. The file wins: it is
// the deliberate, versioned artifact, while the inline form is a quick override.
func MergeRenames(spec FieldSpec, fromFile map[string]string) FieldSpec {
	if len(fromFile) == 0 {
		return spec
	}
	if spec.renames == nil {
		spec.renames = make(map[string]string, len(fromFile))
	}
	for k, v := range fromFile {
		spec.renames[k] = v
	}
	return spec
}

// reshapeSource applies a field selection and rename map to an export source.
// Unknown column names are an error rather than a silently empty column: a typo
// in an import schema should fail loudly, not produce a file the site accepts
// with every value in the wrong place.
func ReshapeSource(src ExportSource, spec FieldSpec) (ExportSource, error) {
	if len(spec.fields) == 0 && len(spec.renames) == 0 {
		return src, nil
	}
	pos := make(map[string]int, len(src.header))
	for i, h := range src.header {
		pos[h] = i
	}
	fields := spec.fields
	if len(fields) == 0 {
		// Rename-only: keep the source order and its completeness.
		fields = src.header
	}
	idx := make([]int, 0, len(fields))
	header := make([]string, 0, len(fields))
	for _, f := range fields {
		i, ok := pos[f]
		if !ok {
			return src, fmt.Errorf("unknown field %q (available: %s)", f, strings.Join(src.header, ", "))
		}
		idx = append(idx, i)
		name := f
		if renamed, has := spec.renames[f]; has && renamed != "" {
			name = renamed
		}
		header = append(header, name)
	}
	fetch := func(page int) ([][]string, error) {
		rows, err := src.fetch(page)
		if err != nil {
			return nil, err
		}
		out := make([][]string, 0, len(rows))
		for _, row := range rows {
			vals := make([]string, len(idx))
			for k, i := range idx {
				if i < len(row) {
					vals[k] = row[i]
				}
			}
			out = append(out, vals)
		}
		return out, nil
	}
	return ExportSource{header: header, fetch: fetch}, nil
}

// streamSource walks an export source page by page, bounded by exportRowLimit.
// It is package-level so the CLI can drive the same sources the web handler
// does without constructing a WebServer.
func streamSource(src ExportSource, emit func([]string) bool) error {
	for page, total := 1, 0; total < exportRowLimit; page++ {
		rows, err := src.fetch(page)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if total >= exportRowLimit {
				return nil
			}
			total++
			if !emit(row) {
				return nil
			}
		}
		if len(rows) < exportPageSize {
			return nil
		}
	}
	return nil
}

// WriteCSV writes header plus rows as RFC 4180 CSV.
func WriteCSV(w io.Writer, src ExportSource) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(src.header); err != nil {
		return err
	}
	var werr error
	err := streamSource(src, func(row []string) bool {
		if e := cw.Write(row); e != nil {
			werr = e
			return false
		}
		return true
	})
	cw.Flush()
	if werr != nil {
		return werr
	}
	if err != nil {
		return err
	}
	return cw.Error()
}

// WriteJSON writes one array of objects. Convenient for a one-shot import, but
// it must buffer a comma decision and cannot be resumed mid-file.
func WriteJSON(w io.Writer, src ExportSource) error {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString("["); err != nil {
		return err
	}
	first := true
	err := streamSource(src, func(row []string) bool {
		b, err := json.Marshal(rowObject(src.header, row))
		if err != nil {
			return false
		}
		if !first {
			if _, err := bw.WriteString(","); err != nil {
				return false
			}
		}
		first = false
		_, err = bw.Write(b)
		return err == nil
	})
	if err != nil {
		return err
	}
	if _, err := bw.WriteString("]\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// WriteJSONL writes one object per line. This is the format to hand to a bulk
// importer: a truncated file is still a valid prefix, and the reader never has
// to hold the whole set in memory.
func WriteJSONL(w io.Writer, src ExportSource) error {
	bw := bufio.NewWriter(w)
	writeErr := error(nil)
	err := streamSource(src, func(row []string) bool {
		b, e := json.Marshal(rowObject(src.header, row))
		if e != nil {
			writeErr = e
			return false
		}
		if _, e := bw.Write(b); e != nil {
			writeErr = e
			return false
		}
		if e := bw.WriteByte('\n'); e != nil {
			writeErr = e
			return false
		}
		return true
	})
	if writeErr != nil {
		return writeErr
	}
	if err != nil {
		return err
	}
	return bw.Flush()
}

// rowObject pairs a header with one row. Short rows simply omit the missing
// columns instead of emitting empty strings the importer would treat as data.
func rowObject(header, row []string) map[string]string {
	obj := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(row) {
			obj[h] = row[i]
		}
	}
	return obj
}

// LimitSource caps an export at n rows. It exists because exportRowLimit is a
// guard rail against a runaway stream, not a batch size: an importer wants the
// first N rows of a filtered set, and a dry run wants a small sample.
func LimitSource(src ExportSource, n int) ExportSource {
	if n <= 0 {
		return src
	}
	emitted := 0
	fetch := func(page int) ([][]string, error) {
		if emitted >= n {
			return nil, nil
		}
		rows, err := src.fetch(page)
		if err != nil {
			return nil, err
		}
		if remaining := n - emitted; len(rows) > remaining {
			rows = rows[:remaining]
		}
		emitted += len(rows)
		return rows, nil
	}
	return ExportSource{header: src.header, fetch: fetch}
}
