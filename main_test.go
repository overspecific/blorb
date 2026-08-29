package main

import (
	"path/filepath"
	"testing"
)

func TestRunNoArgs(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Errorf("run(nil) = %d, want 2", code)
	}
}

func TestRunVersionFlags(t *testing.T) {
	for _, arg := range []string{"version", "-V", "--version"} {
		t.Run(arg, func(t *testing.T) {
			if code := run([]string{arg}); code != 0 {
				t.Errorf("run(%q) = %d, want 0", arg, code)
			}
		})
	}
}

func TestRunHelpFlags(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			if code := run([]string{arg}); code != 0 {
				t.Errorf("run(%q) = %d, want 0", arg, code)
			}
		})
	}
}

func TestRunUnknownCommand(t *testing.T) {
	if code := run([]string{"teleport"}); code != 2 {
		t.Errorf("run(teleport) = %d, want 2", code)
	}
}

func TestCmdChatBadFlags(t *testing.T) {
	if code := cmdChat([]string{"--nope"}); code != 2 {
		t.Errorf("cmdChat(--nope) = %d, want 2", code)
	}
}

func TestCmdChatHelpAndVersion(t *testing.T) {
	if code := cmdChat([]string{"--help"}); code != 0 {
		t.Errorf("cmdChat(--help) = %d, want 0", code)
	}
	if code := cmdChat([]string{"--version"}); code != 0 {
		t.Errorf("cmdChat(--version) = %d, want 0", code)
	}
}

func TestCmdChatMissingConfig(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "blorb.json")

	if code := cmdChat([]string{"-c", missing}); code != 1 {
		t.Errorf("cmdChat(-c %s) = %d, want 1", missing, code)
	}
}
