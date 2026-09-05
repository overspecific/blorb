package usage

import "testing"

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want string
	}{
		{"zero", 0, "0B"},
		{"below a kilobyte", 512, "512B"},
		{"exact kilobyte", 4096, "4KB"},
		{"fractional kilobyte", 4608, "4.5KB"},
		{"exact megabyte", 1048576, "1MB"},
		{"exact gigabyte", 1073741824, "1GB"},
		{"sub-kilobyte fraction stays bytes", 1023, "1023B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := humanBytes(tt.n); got != tt.want {
				t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}
