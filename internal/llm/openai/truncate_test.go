package openai

import "testing"

// TestTruncate pins the error-body truncation helper: short bodies pass
// through unchanged, long ones cut at the limit — backing off to a rune
// boundary so multibyte sequences are not split — and gain a marker.
//
// This file is the openai package's internal test file: the main test
// suite lives in openai_test (external) to exercise the public surface,
// but truncate is unexported, so its test sits beside the code.
func TestTruncate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		data  string
		limit int
		want  string
	}{
		{name: "short body passes through", data: "small", limit: 10, want: "small"},
		{name: "exactly the limit passes through", data: "12345", limit: 5, want: "12345"},
		{
			name:  "ascii cut at the limit",
			data:  "abcdefghij",
			limit: 4,
			want:  "abcd...(truncated)",
		},
		{
			name:  "multibyte rune not split",
			data:  "héllo",
			limit: 2,
			want:  "h...(truncated)",
		},
		{
			name:  "cut landing mid-rune backs off",
			data:  "éééé",
			limit: 3,
			want:  "é...(truncated)",
		},
		{
			name:  "empty body",
			data:  "",
			limit: 4,
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := truncate([]byte(tc.data), tc.limit)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.data, tc.limit, got, tc.want)
			}
		})
	}
}
