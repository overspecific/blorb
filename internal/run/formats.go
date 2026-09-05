package run

import (
	"fmt"
	"io"
	"strings"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
	"github.com/overspecific/blorb/internal/usage"
)

// Output format values for Options.Format.
const (
	// FormatChat is the default: the chat-style stream, everything on
	// stdout.
	FormatChat = "chat"
	// FormatPlain puts only the agent's output on stdout; every other
	// byte (headings, tool activity, streaming, footers) goes to stderr.
	FormatPlain = "plain"
	// FormatNDJSON streams the full event stream as one JSON object per
	// line on stdout.
	FormatNDJSON = "ndjson"
)

// stderrOr returns o.Stderr, or o.Stdout when unset (the single-stream
// fallback).
func (o Options) stderrOr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return o.Stdout
}

// diagnostics returns the writer for the run's diagnostic lines: the
// usage footer and the "stopped by platform" notices. Chat keeps them on
// stdout (its single stream, byte-identical to the pre-format behavior);
// the other formats treat stderr as the diagnostics channel.
func (o Options) diagnostics() io.Writer {
	if o.Format == FormatChat || o.Format == "" {
		return o.Stdout
	}
	return o.stderrOr()
}

// events builds the run's event callbacks for the output format.
// chat renders the chat-style stream on stdout; plain renders the same
// chat-style stream on stderr and tees only assistant text to stdout;
// ndjson streams flat typed JSON events on stdout. The returned finish
// callback (ndjson only) emits the stream's terminal done/error event.
func (o Options) events(account *usage.Account) (printEvent func(engine.Event) error, onSubagent func(tools.SubagentEvent) error, flush func(), finish func(final string, runErr error) error) {
	switch o.Format {
	case FormatNDJSON:
		sink := newNDJSONSink(o.Stdout, account)
		return sink.printEvent, sink.onSubagent, func() {}, sink.finish
	case FormatPlain:
		diagPrint, diagSubagent, flush := chat.Events(o.stderrOr(), o.ToolOutput)
		printEvent = func(ev engine.Event) error {
			if err := diagPrint(ev); err != nil {
				return err
			}
			// Only the assistant's own text is the run's output. The
			// engine suppresses whole-message events when it streams, so
			// the two kinds never double-write.
			switch ev.Kind {
			case engine.EventAssistantText, engine.EventAssistantTextDelta:
				if _, err := o.Stdout.Write([]byte(ev.Text)); err != nil {
					return err
				}
				// The logprob block prints after the whole response body;
				// streamed responses carry no logprobs, so a streamed run
				// with the flag prints nothing here.
				if o.ShowLogprobs && ev.Kind == engine.EventAssistantText && len(ev.Logprobs) > 0 {
					printLogprobs(o.Stdout, ev.Logprobs)
				}
			}
			return nil
		}
		return printEvent, diagSubagent, flush, nil
	default:
		printEvent, onSubagent, flush := chat.Events(o.Stdout, o.ToolOutput)
		return printEvent, onSubagent, flush, nil
	}
}

// printLogprobs writes the logprob block: one line per token after the
// response body — the token, its logprob, and, compactly, the top
// alternative when present.
func printLogprobs(w io.Writer, lps []llm.Logprob) {
	for _, lp := range lps {
		if len(lp.Top) > 0 {
			fmt.Fprintf(w, "  %q logprob=%.4f (top: %q %.4f)\n", lp.Token, lp.Logprob, lp.Top[0].Token, lp.Top[0].Logprob)
			continue
		}
		fmt.Fprintf(w, "  %q logprob=%.4f\n", lp.Token, lp.Logprob)
	}
}

// validateFormat reports whether the format is one of the supported
// values.
func (o Options) validateFormat() error {
	switch o.Format {
	case FormatChat, FormatPlain, FormatNDJSON, "":
		return nil
	}
	supported := strings.Join([]string{FormatChat, FormatPlain, FormatNDJSON}, ", ")
	return fmt.Errorf("run: unknown format %q (supported: %s)", o.Format, supported)
}
