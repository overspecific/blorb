package voice

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureReadsAndClosesClean(t *testing.T) {
	r, err := startCapture(context.Background(), []string{"sh", "-c", "printf 'abc'"})
	if err != nil {
		t.Fatalf("startCapture error = %v, want nil", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll error = %v, want nil", err)
	}
	if string(got) != "abc" {
		t.Errorf("capture output = %q, want %q", got, "abc")
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close error = %v, want nil", err)
	}
}

func TestCaptureStartError(t *testing.T) {
	if _, err := startCapture(context.Background(), []string{"/nonexistent/blorb-audio"}); err == nil {
		t.Fatal("startCapture error = nil, want a start error")
	}
}

func TestCaptureEmptyCommand(t *testing.T) {
	if _, err := startCapture(context.Background(), nil); err == nil {
		t.Fatal("startCapture error = nil, want an empty command error")
	}
}

func TestCaptureExitReported(t *testing.T) {
	r, err := startCapture(context.Background(), []string{"sh", "-c", "exit 3"})
	if err != nil {
		t.Fatalf("startCapture error = %v, want nil", err)
	}
	// Drain so the process reaps before Close.
	_, _ = io.Copy(io.Discard, r)
	err = r.Close()
	if err == nil {
		t.Fatal("Close error = nil, want the non-zero exit status")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("Close error = %v, want it to name exit status 3", err)
	}
}

func TestPlaybackWritesAndCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.pcm")
	w, err := startPlayback(context.Background(), []string{"sh", "-c", "cat > " + path})
	if err != nil {
		t.Fatalf("startPlayback error = %v, want nil", err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write error = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read playback output: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("playback received %q, want %q", got, "hello")
	}
}

func TestPlaybackExitReported(t *testing.T) {
	w, err := startPlayback(context.Background(), []string{"sh", "-c", "exit 4"})
	if err != nil {
		t.Fatalf("startPlayback error = %v, want nil", err)
	}
	err = w.Close()
	if err == nil {
		t.Fatal("Close error = nil, want the non-zero exit status")
	}
	if !strings.Contains(err.Error(), "exit status 4") {
		t.Errorf("Close error = %v, want it to name exit status 4", err)
	}
}

func TestPlaybackStartError(t *testing.T) {
	if _, err := startPlayback(context.Background(), []string{"/nonexistent/blorb-audio"}); err == nil {
		t.Fatal("startPlayback error = nil, want a start error")
	}
}

func TestPlaybackEmptyCommand(t *testing.T) {
	if _, err := startPlayback(context.Background(), nil); err == nil {
		t.Fatal("startPlayback error = nil, want an empty command error")
	}
}

func TestCloseTwiceIsSafe(t *testing.T) {
	t.Run("capture", func(t *testing.T) {
		r, err := startCapture(context.Background(), []string{"sh", "-c", "sleep 5"})
		if err != nil {
			t.Fatalf("startCapture error = %v, want nil", err)
		}
		if err := r.Close(); err != nil {
			t.Errorf("first Close error = %v, want nil", err)
		}
		if err := r.Close(); err != nil {
			t.Errorf("second Close error = %v, want nil", err)
		}
	})
	t.Run("playback", func(t *testing.T) {
		w, err := startPlayback(context.Background(), []string{"sh", "-c", "sleep 5"})
		if err != nil {
			t.Fatalf("startPlayback error = %v, want nil", err)
		}
		if err := w.Close(); err != nil {
			t.Errorf("first Close error = %v, want nil", err)
		}
		if err := w.Close(); err != nil {
			t.Errorf("second Close error = %v, want nil", err)
		}
	})
}
