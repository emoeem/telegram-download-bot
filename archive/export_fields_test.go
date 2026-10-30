package archive

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
)

// staticSource builds an export source from fixed rows, which is all the
// reshaping logic needs to be exercised.
func staticSource(header []string, rows [][]string) ExportSource {
	return ExportSource{header: header, fetch: func(page int) ([][]string, error) {
		if page > 1 {
			return nil, nil
		}
		return rows, nil
	}}
}

func TestParseFieldSpec(t *testing.T) {
	spec, err := ParseFieldSpec("id, title:name ,body:content,")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.fields) != 3 {
		t.Fatalf("fields=%v", spec.fields)
	}
	if spec.renames["title"] != "name" || spec.renames["body"] != "content" {
		t.Fatalf("renames=%v", spec.renames)
	}
	if _, has := spec.renames["id"]; has {
		t.Fatalf("id has no rename, got %v", spec.renames)
	}
}

func TestParseFieldSpecRejectsEmptyName(t *testing.T) {
	if _, err := ParseFieldSpec(":name"); err == nil {
		t.Fatal("an empty source name must be an error, not a silent skip")
	}
}

func TestReshapeSourceSelectsAndReorders(t *testing.T) {
	src := staticSource([]string{"a", "b", "c"}, [][]string{{"1", "2", "3"}})
	spec, err := ParseFieldSpec("c,a")
	if err != nil {
		t.Fatal(err)
	}
	out, err := ReshapeSource(src, spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out.header, ","); got != "c,a" {
		t.Fatalf("header=%q", got)
	}
	rows, err := out.fetch(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rows[0], ","); got != "3,1" {
		t.Fatalf("row=%q, want the reordered values", got)
	}
}

func TestReshapeSourceRenameOnlyKeepsOrder(t *testing.T) {
	// A mapping file without a field list must not drop columns: an operator
	// renaming for an importer still wants every column.
	src := staticSource([]string{"a", "b"}, [][]string{{"1", "2"}})
	out, err := ReshapeSource(src, FieldSpec{renames: map[string]string{"b": "bee"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out.header, ","); got != "a,bee" {
		t.Fatalf("header=%q", got)
	}
}

func TestReshapeSourceUnknownField(t *testing.T) {
	src := staticSource([]string{"a"}, [][]string{{"1"}})
	spec, _ := ParseFieldSpec("nope")
	if _, err := ReshapeSource(src, spec); err == nil {
		t.Fatal("an unknown field must fail: a typo in an import schema is otherwise silent")
	}
}

func TestReshapeSourceEmptySpecIsIdentity(t *testing.T) {
	src := staticSource([]string{"a", "b"}, [][]string{{"1", "2"}})
	out, err := ReshapeSource(src, FieldSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.header) != 2 {
		t.Fatalf("header=%v", out.header)
	}
}

func TestWriteCSVShape(t *testing.T) {
	src := staticSource([]string{"id", "text"}, [][]string{
		{"1", "line one\nline two"}, // embedded newline must be quoted
		{"2", "a,b"},                // embedded comma must be quoted
	})
	var buf bytes.Buffer
	if err := WriteCSV(&buf, src); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want header + 2", len(records))
	}
	if records[1][1] != "line one\nline two" || records[2][1] != "a,b" {
		t.Fatalf("round trip lost data: %v", records)
	}
}

func TestWriteJSONLOneObjectPerLine(t *testing.T) {
	src := staticSource([]string{"id", "body"}, [][]string{
		{"1", "first"},
		{"2", "second"},
	})
	var buf bytes.Buffer
	if err := WriteJSONL(&buf, src); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	var obj map[string]string
	if err := json.Unmarshal([]byte(lines[1]), &obj); err != nil {
		t.Fatalf("line is not a JSON object: %v", err)
	}
	if obj["id"] != "2" || obj["body"] != "second" {
		t.Fatalf("obj=%v", obj)
	}
}

func TestWriteJSONIsValidArray(t *testing.T) {
	src := staticSource([]string{"id"}, [][]string{{"1"}, {"2"}})
	var buf bytes.Buffer
	if err := WriteJSON(&buf, src); err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("not a JSON array: %v (%s)", err, buf.String())
	}
	if len(out) != 2 {
		t.Fatalf("len=%d", len(out))
	}
}

func TestMergeRenamesFileWins(t *testing.T) {
	spec, _ := ParseFieldSpec("id:inline")
	merged := MergeRenames(spec, map[string]string{"id": "fromfile"})
	if merged.renames["id"] != "fromfile" {
		t.Fatalf("renames=%v, want the file mapping to win", merged.renames)
	}
}
