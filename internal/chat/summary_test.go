package chat

import "testing"

// TestOutputSummary pins the tool-result count summary used when tool
// output is suppressed: character count (runes, not bytes), line count
// (a trailing newline does not count as an extra line), and pluralization.
func TestOutputSummary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   string
	}{
		{"empty", "", "0 characters, 0 lines"},
		{"single line no newline", "hello", "5 characters, 1 line"},
		{"single line with newline", "hello\n", "6 characters, 1 line"},
		{"two lines no trailing newline", "one\ntwo", "7 characters, 2 lines"},
		{"two lines with trailing newline", "one\ntwo\n", "8 characters, 2 lines"},
		{"four lines", "one\ntwo\nthree\nfour\n", "19 characters, 4 lines"},
		{"only a newline", "\n", "1 characters, 1 line"},
		{"unicode counted as runes", "héllo\n", "6 characters, 1 line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outputSummary(tc.output); got != tc.want {
				t.Errorf("outputSummary(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}
