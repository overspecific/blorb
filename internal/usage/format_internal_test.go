package usage

import (
	"testing"
	"time"
)

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

func TestHumanDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"whole seconds", 4 * time.Second, "4s"},
		{"one decimal", 12345 * time.Millisecond, "12.3s"},
		{"trailing .0 dropped", 90 * time.Second, "90s"},
		{"sub-second keeps one decimal", 220 * time.Millisecond, "0.2s"},
		{"minutes stay seconds", 90 * time.Second, "90s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := humanDuration(tt.d); got != tt.want {
				t.Errorf("humanDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}
