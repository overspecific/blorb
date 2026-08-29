package cli_test

import (
	"testing"

	"github.com/overspecific/blorb/internal/cli"
)

func TestParseChatFlagsDefaults(t *testing.T) {
	t.Parallel()

	flags, err := cli.ParseChatFlags(nil)
	if err != nil {
		t.Fatalf("ParseChatFlags(nil) error = %v, want nil", err)
	}
	if got, want := flags.ConfigPath, cli.DefaultConfigPath; got != want {
		t.Errorf("ConfigPath = %q, want %q", got, want)
	}
}

func TestParseChatFlagsConfigPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"double dash separated", []string{"--config", "foo.json"}, "foo.json"},
		{"double dash equals", []string{"--config=foo.json"}, "foo.json"},
		{"single dash separated", []string{"-c", "foo.json"}, "foo.json"},
		{"single dash equals", []string{"-c=foo.json"}, "foo.json"},
		{"single dash attached", []string{"-cfoo.json"}, "foo.json"},
		{"after other flags", []string{"-V", "--config", "x.json"}, "x.json"},
		{"before other flags", []string{"--config", "x.json", "-V"}, "x.json"},
		{"spaces preserved", []string{"--config", "my dir/x.json"}, "my dir/x.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			flags, err := cli.ParseChatFlags(tc.args)
			if err != nil {
				t.Fatalf("ParseChatFlags(%v) error = %v, want nil", tc.args, err)
			}
			if flags.ConfigPath != tc.want {
				t.Errorf("ConfigPath = %q, want %q", flags.ConfigPath, tc.want)
			}
		})
	}
}

func TestParseChatFlagsVersionAndHelp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		version bool
		help    bool
	}{
		{"long version", []string{"--version"}, true, false},
		{"short version", []string{"-V"}, true, false},
		{"long help", []string{"--help"}, false, true},
		{"short help", []string{"-h"}, false, true},
		{"combined shorts", []string{"-hV"}, true, true},
		{"combined shorts reversed", []string{"-Vh"}, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			flags, err := cli.ParseChatFlags(tc.args)
			if err != nil {
				t.Fatalf("ParseChatFlags(%v) error = %v, want nil", tc.args, err)
			}
			if flags.ShowVersion != tc.version {
				t.Errorf("ShowVersion = %v, want %v", flags.ShowVersion, tc.version)
			}
			if flags.ShowHelp != tc.help {
				t.Errorf("ShowHelp = %v, want %v", flags.ShowHelp, tc.help)
			}
		})
	}
}

func TestParseChatFlagsTerminator(t *testing.T) {
	t.Parallel()

	flags, err := cli.ParseChatFlags([]string{"--"})
	if err != nil {
		t.Fatalf("ParseChatFlags error = %v, want nil", err)
	}
	if flags.ConfigPath != cli.DefaultConfigPath {
		t.Errorf("ConfigPath = %q, want the default", flags.ConfigPath)
	}
}

func TestParseChatFlagsErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown long flag", []string{"--nope"}, `unknown flag --nope`},
		{"unknown short flag", []string{"-x"}, `unknown flag -x`},
		{"unknown combined flag", []string{"-hx"}, `unknown flag -x`},
		{"missing config value long", []string{"--config"}, "--config requires a path"},
		{"missing config value short", []string{"-c"}, "-c requires a path"},
		{"empty config equals", []string{"--config="}, "--config requires a non-empty path"},
		{"empty config attached", []string{"-c="}, "-c requires a path"},
		{"positional argument", []string{"stray"}, `unexpected argument "stray"`},
		{"positional after terminator", []string{"--", "stray"}, `unexpected argument "stray"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := cli.ParseChatFlags(tc.args)
			if err == nil {
				t.Fatalf("ParseChatFlags(%v) succeeded, want an error", tc.args)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}
