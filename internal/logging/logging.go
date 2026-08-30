// Package logging implements best-effort wire logging of LLM requests and
// responses and tool calls and results: one file per interaction, named
// timestamp-first so sorting the filenames replays a turn in order.
//
// Logging is best-effort by contract: producers call Sink.Write for every
// record and treat any error as informational only. Logging failures never
// fail the operation being logged.
package logging

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind identifies what kind of interaction a Record captures. Kind values
// double as filename components and header labels.
type Kind string

const (
	KindLLMRequest  Kind = "llm-request"
	KindLLMResponse Kind = "llm-response"
	KindToolRequest Kind = "tool-request"
	KindToolResult  Kind = "tool-result"
)

// Record is one captured wire interaction. Callers must set Time (at
// capture time — when the interaction happened, not when it is written)
// and Kind; the sink handles uniqueness and ordering.
type Record struct {
	// Time is when the interaction happened. When zero, the file sink
	// stamps the write time instead.
	Time time.Time
	Kind Kind
	// Method is the HTTP method, or "RUN" for tool records.
	Method string
	// URL is the request URL, or the tool name for tool records.
	URL string
	// Headers carries request or response headers. Tool records
	// typically have none.
	Headers map[string][]string
	// Body is the exact wire bytes. It is written verbatim, never
	// re-encoded; the record owns its copy.
	Body []byte
}

// Sink receives logging records. Implementations must be safe for
// concurrent use.
//
// The contract is best-effort: producers write a record for every
// interaction and treat an error as informational only — logging is the one
// place errors are not propagated.
type Sink interface {
	Write(r Record) error
}

// nopSink discards all records.
type nopSink struct{}

// NewNop returns a sink that discards all records.
func NewNop() Sink {
	return nopSink{}
}

func (nopSink) Write(Record) error { return nil }

// Buffered is an in-memory sink retaining the last size records (all
// records when size <= 0). It is safe for concurrent use.
type Buffered struct {
	mu      sync.Mutex
	size    int
	records []Record
}

// NewBuffered returns an in-memory sink retaining the last size records, or
// all records when size <= 0.
func NewBuffered(size int) *Buffered {
	return &Buffered{size: size}
}

func (b *Buffered) Write(r Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = append(b.records, cloneRecord(r))
	if b.size > 0 && len(b.records) > b.size {
		b.records = b.records[len(b.records)-b.size:]
	}
	return nil
}

// Records returns a copy of the retained records in write order.
func (b *Buffered) Records() []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Record, len(b.records))
	for i, r := range b.records {
		out[i] = cloneRecord(r)
	}
	return out
}

func cloneRecord(r Record) Record {
	c := r
	c.Body = append([]byte(nil), r.Body...)
	if r.Headers != nil {
		c.Headers = make(map[string][]string, len(r.Headers))
		for k, v := range r.Headers {
			c.Headers[k] = append([]string(nil), v...)
		}
	}
	return c
}

// timeDirName renders a UTC time as <date>-<nanos>: six date digits, six
// time digits, then a dash and nine fixed fractional digits. Go's layout
// machinery cannot express fixed-width fractional seconds (its fractional
// directives trim trailing zeros), so the nanos are formatted directly. The
// fixed width keeps filenames unambiguous and lexically sortable.
func timeDirName(t time.Time) string {
	return t.Format("20060102T150405") + fmt.Sprintf("-%09d", t.Nanosecond())
}

// fileSink writes each record as exactly one file in a directory.
type fileSink struct {
	dir string

	mu       sync.Mutex
	lastTime time.Time
}

// New returns a file sink writing one file per record into dir, which must
// already exist (creating it is the caller's job). The returned error is
// safe to treat as informational by producers.
func New(dir string) (Sink, error) {
	if err := validateDir(dir); err != nil {
		return nil, err
	}
	return &fileSink{dir: dir}, nil
}

// validateDir checks that dir exists and is a directory.
func validateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat log dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("log dir %s is not a directory", dir)
	}
	return nil
}

// NewSession creates a fresh per-conversation subdirectory under dir,
// named <timestamp>-<uuid> (timestamp in the same form as log filenames,
// so session directories sort chronologically; hex UUID from crypto/rand,
// so sessions started in the same second never collide), and returns a
// file sink for it alongside the name. Unlike Sink.Write, an error here is
// startup-fatal for the caller: this runs before any turn, and a session
// that cannot prepare its logs should not silently run unlogged.
func NewSession(dir string) (Sink, string, error) {
	if err := validateDir(dir); err != nil {
		return nil, "", err
	}

	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return nil, "", fmt.Errorf("generate session id: %w", err)
	}
	name := timeDirName(time.Now()) + "-" + hex.EncodeToString(uuid[:])

	sub := filepath.Join(dir, name)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return nil, "", fmt.Errorf("create session dir %s: %w", sub, err)
	}
	sink, err := New(sub)
	if err != nil {
		return nil, "", err
	}
	return sink, name, nil
}

func (s *fileSink) Write(r Record) error {
	name, normalized, err := s.nextFilename(r)
	if err != nil {
		return err
	}
	content := render(r.Kind, normalized, r)
	return os.WriteFile(filepath.Join(s.dir, name), content, 0o600)
}

// nextFilename returns the unique filename for r and the time actually used
// (bumped when records collide or arrive out of order).
func (s *fileSink) nextFilename(r Record) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := r.Time
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if !now.After(s.lastTime) && !s.lastTime.IsZero() {
		now = s.lastTime.Add(time.Nanosecond)
	}

	kind := string(r.Kind)
	if kind == "" || strings.ContainsAny(kind, "/\\") || kind == "." || kind == ".." {
		return "", time.Time{}, fmt.Errorf("invalid record kind %q", r.Kind)
	}

	s.lastTime = now
	return timeDirName(now) + "-" + kind + ".txt", now, nil
}

// rfc3339NanoUTC renders t (already UTC) with an explicit +00:00 offset:
// RFC3339Nano's Z suffix is compact but the explicit form is unambiguous
// and matches other tooling.
func rfc3339NanoUTC(t time.Time) string {
	return t.Format("2006-01-02T15:04:05.999999999-07:00")
}

// render formats a record as its file content: a "# <time> kind=<kind>"
// line, a "<Method> <URL>" line, sorted header lines, a blank line, then
// the body verbatim. An empty body omits the blank separator.
func render(kind Kind, t time.Time, r Record) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# %s kind=%s\n", rfc3339NanoUTC(t), kind)
	fmt.Fprintf(&buf, "%s %s\n", r.Method, r.URL)

	keys := make([]string, 0, len(r.Headers))
	for k := range r.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&buf, "%s: %s\n", k, strings.Join(r.Headers[k], ", "))
	}

	if len(r.Body) > 0 {
		buf.WriteByte('\n')
		buf.Write(r.Body)
	}
	return buf.Bytes()
}
