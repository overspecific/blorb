package run_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/run"
)

func TestResolvePrompt(t *testing.T) {
	t.Parallel()

	writeFile := func(t *testing.T, dir, name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	tests := []struct {
		name    string
		arg     string
		stdin   string
		setup   func(t *testing.T, dir string) string // returns the arg to use
		want    string
		wantErr string
	}{
		{
			name: "literal arg verbatim including spaces",
			arg:  "  hello  ",
			want: "  hello  ",
		},
		{
			name: "double-at escape strips one @",
			arg:  "@@hi",
			want: "@hi",
		},
		{
			name: "double-at alone yields literal @",
			arg:  "@@",
			want: "@",
		},
		{
			name: "double-at keeps spaces",
			arg:  "@@ spaced ",
			want: "@ spaced ",
		},
		{
			name: "at-file reads and trims",
			setup: func(t *testing.T, dir string) string {
				return "@" + writeFile(t, dir, "prompt.txt", "hello\n")
			},
			want: "hello",
		},
		{
			name: "at-file with only whitespace is an error",
			setup: func(t *testing.T, dir string) string {
				return "@" + writeFile(t, dir, "blank.txt", "  \n\t\n")
			},
			wantErr: "run: empty prompt from @file",
		},
		{
			name:    "at-file missing",
			arg:     "@/definitely/not/here/prompt.txt",
			wantErr: "run: read prompt file:",
		},
		{
			name:    "bare at has no path",
			arg:     "@",
			wantErr: "run: @file reference has no path",
		},
		{
			name:  "at-dash reads stdin",
			arg:   "@-",
			stdin: "piped\n",
			want:  "piped",
		},
		{
			name:    "at-dash with empty stdin",
			arg:     "@-",
			stdin:   "",
			wantErr: "run: empty prompt from stdin",
		},
		{
			name:  "dash reads stdin",
			arg:   "-",
			stdin: "hi there\n",
			want:  "hi there",
		},
		{
			name:    "dash with empty stdin",
			arg:     "-",
			stdin:   "",
			wantErr: "run: empty prompt from stdin",
		},
		{
			name:    "dash with only whitespace stdin",
			arg:     "-",
			stdin:   "   \n\t \n",
			wantErr: "run: empty prompt from stdin",
		},
		{
			name:    "empty arg is an error, not stdin",
			arg:     "",
			stdin:   "piped\n",
			wantErr: "run: no prompt given (pass a prompt argument, @file, or - for stdin)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			arg := tt.arg
			if tt.setup != nil {
				arg = tt.setup(t, t.TempDir())
			}

			got, err := run.ResolvePrompt(arg, strings.NewReader(tt.stdin))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ResolvePrompt(%q) = %q, want error containing %q", arg, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePrompt(%q) error = %v, want nil", arg, err)
			}
			if got != tt.want {
				t.Errorf("ResolvePrompt(%q) = %q, want %q", arg, got, tt.want)
			}
		})
	}
}

func TestResolvePromptMissingFileWrapsOSError(t *testing.T) {
	t.Parallel()

	_, err := run.ResolvePrompt("@/no/such/file/here.txt", strings.NewReader(""))
	if err == nil {
		t.Fatal("ResolvePrompt on a missing file succeeded, want an error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap the os error (ErrNotExist)", err)
	}
}
