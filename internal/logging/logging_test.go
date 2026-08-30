package logging_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/logging"
)

func sampleRecord() logging.Record {
	return logging.Record{
		Time:    time.Date(2026, 8, 30, 14, 25, 33, 123456789, time.UTC),
		Kind:    logging.KindLLMRequest,
		Method:  "POST",
		URL:     "http://localhost:13305/v1/chat/completions",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    []byte(`{"model":"gpt-4o-mini"}`),
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestFileSinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sink, err := logging.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r := sampleRecord()
	if err := sink.Write(r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	names := dirNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("got %d files, want 1: %v", len(names), names)
	}

	nameRe := regexp.MustCompile(`^\d{8}T\d{6}-\d{9}-llm-request\.txt$`)
	if !nameRe.MatchString(names[0]) {
		t.Fatalf("filename %q does not match pattern", names[0])
	}

	data, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	want := "# 2026-08-30T14:25:33.123456789+00:00 kind=llm-request\n" +
		"POST http://localhost:13305/v1/chat/completions\n" +
		"Content-Type: application/json\n" +
		"\n" +
		`{"model":"gpt-4o-mini"}`
	if got := string(data); got != want {
		t.Fatalf("file content mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestFileSinkHeaderFormatting(t *testing.T) {
	dir := t.TempDir()
	sink, err := logging.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r := logging.Record{
		Time: time.Date(2026, 8, 30, 14, 25, 33, 0, time.UTC),
		Kind: logging.KindLLMResponse,
		URL:  "http://localhost/x",
		Headers: map[string][]string{
			"Z-Last":  {"b", "c"},
			"A-First": {"a"},
			"Multi":   {"x1", "x2", "x3"},
		},
	}
	if err := sink.Write(r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, dirNames(t, dir)[0]))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(string(data), "\n")
	if lines[0] != "# 2026-08-30T14:25:33+00:00 kind=llm-response" {
		t.Fatalf("bad header line: %q", lines[0])
	}
	if lines[1] != " http://localhost/x" {
		t.Fatalf("bad request line: %q", lines[1])
	}
	// Header keys sort alphabetically; multi-value headers join with ", ".
	// The body is empty here, so there is no blank separator line.
	want := "A-First: a\nMulti: x1, x2, x3\nZ-Last: b, c\n"
	if got := strings.Join(lines[2:], "\n"); got != want {
		t.Fatalf("bad header block:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFileSinkEmptyBody(t *testing.T) {
	dir := t.TempDir()
	sink, err := logging.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r := sampleRecord()
	r.Body = nil
	if err := sink.Write(r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, dirNames(t, dir)[0]))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	want := "# 2026-08-30T14:25:33.123456789+00:00 kind=llm-request\n" +
		"POST http://localhost:13305/v1/chat/completions\n" +
		"Content-Type: application/json\n"
	if got := string(data); got != want {
		t.Fatalf("empty body should write no blank separator:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestNewRejectsBadDirs(t *testing.T) {
	t.Run("nonexistent directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		_, err := logging.New(dir)
		if err == nil {
			t.Fatal("expected error for nonexistent dir")
		}
		if !strings.Contains(err.Error(), "missing") {
			t.Fatalf("error should mention the path: %v", err)
		}
	})

	t.Run("regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := logging.New(file)
		if err == nil {
			t.Fatal("expected error for regular file")
		}
	})
}

func TestNewNopWritesNothing(t *testing.T) {
	dir := t.TempDir()
	sink := logging.NewNop()
	for range 3 {
		if err := sink.Write(sampleRecord()); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("nop sink wrote files: %v", names)
	}
}

func TestFileSinkOrdering(t *testing.T) {
	dir := t.TempDir()
	sink, err := logging.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	same := time.Date(2026, 8, 30, 14, 25, 33, 1000, time.UTC)
	earlier := same.Add(-time.Second)

	recs := []logging.Record{
		{Time: same, Kind: logging.KindLLMRequest, URL: "http://a"},
		{Time: same, Kind: logging.KindLLMResponse, URL: "http://a"},
		{Time: earlier, Kind: logging.KindToolRequest, URL: "echo"},
	}
	for i, r := range recs {
		if err := sink.Write(r); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	names := dirNames(t, dir)
	if len(names) != 3 {
		t.Fatalf("got %d files, want 3: %v", len(names), names)
	}

	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := range names {
		if names[i] != sorted[i] {
			t.Fatalf("files on disk were not lexically sorted: %v", names)
		}
	}

	// The sink bumps times so filenames are unique and strictly
	// increasing: the same-nanosecond pair lands first, and the record
	// written last with an earlier Time is bumped above both.
	wantKinds := []logging.Kind{
		logging.KindLLMRequest,
		logging.KindLLMResponse,
		logging.KindToolRequest,
	}
	for i, want := range wantKinds {
		if !strings.HasSuffix(names[i], "-"+string(want)+".txt") {
			t.Fatalf("sorted file %d: got %q, want suffix -%s.txt", i, names[i], want)
		}
	}

	// Timestamps are strictly increasing between the collisions.
	stamps := make([]string, len(names))
	for i, name := range names {
		stamps[i] = strings.SplitN(name, "-", 3)[0] + "-" + strings.SplitN(name, "-", 3)[1]
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i] <= stamps[i-1] {
			t.Fatalf("timestamps not strictly increasing: %v", stamps)
		}
	}
}

func TestFileSinkZeroTime(t *testing.T) {
	dir := t.TempDir()
	sink, err := logging.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r := sampleRecord()
	r.Time = time.Time{}
	if err := sink.Write(r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	names := dirNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("got %d files, want 1: %v", len(names), names)
	}
	nameRe := regexp.MustCompile(`^\d{8}T\d{6}-\d{9}-llm-request\.txt$`)
	if !nameRe.MatchString(names[0]) {
		t.Fatalf("filename %q does not match pattern", names[0])
	}
}

func TestFileSinkRejectsBadKind(t *testing.T) {
	dir := t.TempDir()
	sink, err := logging.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r := sampleRecord()
	r.Kind = "../evil"
	if err := sink.Write(r); err == nil {
		t.Fatal("expected error for kind containing a path separator")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("rejected record left files behind: %v", names)
	}
}

func TestBufferedSink(t *testing.T) {
	t.Run("retains all records when size <= 0", func(t *testing.T) {
		sink := logging.NewBuffered(0)
		for i := range 5 {
			r := sampleRecord()
			r.Kind = logging.Kind(fmt.Sprintf("kind-%d", i))
			if err := sink.Write(r); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		recs := sink.Records()
		if len(recs) != 5 {
			t.Fatalf("got %d records, want 5", len(recs))
		}
		for i, r := range recs {
			want := logging.Kind(fmt.Sprintf("kind-%d", i))
			if r.Kind != want {
				t.Fatalf("record %d: got kind %q, want %q (write order broken)", i, r.Kind, want)
			}
		}
	})

	t.Run("retains only last size records", func(t *testing.T) {
		sink := logging.NewBuffered(2)
		for i := range 5 {
			r := sampleRecord()
			r.Kind = logging.Kind(fmt.Sprintf("kind-%d", i))
			if err := sink.Write(r); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		recs := sink.Records()
		if len(recs) != 2 {
			t.Fatalf("got %d records, want 2", len(recs))
		}
		if recs[0].Kind != "kind-3" || recs[1].Kind != "kind-4" {
			t.Fatalf("buffered sink kept wrong records: %v", recs)
		}
	})

	t.Run("Records returns a copy", func(t *testing.T) {
		sink := logging.NewBuffered(0)
		if err := sink.Write(sampleRecord()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		recs := sink.Records()
		recs[0] = logging.Record{Kind: "mutated"}
		if got := sink.Records()[0].Kind; got != logging.KindLLMRequest {
			t.Fatalf("mutating Records() result affected the sink: got kind %q", got)
		}
	})

	t.Run("returned record bodies are copied", func(t *testing.T) {
		sink := logging.NewBuffered(0)
		if err := sink.Write(sampleRecord()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		records := sink.Records()
		records[0].Body[0] = 'X'
		if got := sink.Records()[0].Body[0]; got == 'X' {
			t.Fatal("mutating a returned record's body affected the sink")
		}
	})
}
