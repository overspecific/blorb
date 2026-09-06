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
// ndjson streams flat typed JSON events on stdout. onJudge streams the
// judge's thinking and tool activity live (stdout for chat, stderr for
// plain, the sink's emitter for ndjson); the judgement itself prints
// as blocks from the outcomes after the chain completes. onJudgeError
// emits the non-terminal judge_error line (ndjson only). The returned
// finish callback (ndjson only) emits the stream's terminal done/error
// event. Both chat and plain print the per-token logprob block after a
// whole assistant message when Logprobs is on (streamed responses
// carry no logprobs, and a --logprobs run cannot stream).
func (o Options) events(account *usage.Account) (printEvent func(engine.Event) error, onSubagent func(tools.SubagentEvent) error, onJudge func(tools.JudgeEvent) error, onJudgeError func(judge string, jErr error) error, flush func(), finish func(final string, runErr error) error) {
	switch o.Format {
	case FormatNDJSON:
		sink := newNDJSONSink(o.Stdout, account)
		return sink.printEvent, sink.onSubagent, sink.onJudge, sink.onJudgeError, func() {}, sink.finish
	case FormatPlain:
		diagPrint, diagSubagent, flush := chat.Events(o.stderrOr(), o.ToolOutput)
		printEvent := o.logprobTee(diagPrint, func(ev engine.Event) error {
			// Only the assistant's own text is the run's output.
			_, err := o.Stdout.Write([]byte(ev.Text))
			return err
		})
		judgePrint, _ := chat.JudgeEvents(o.stderrOr(), o.ToolOutput)
		return printEvent, diagSubagent, judgePrint, nil, flush, nil
	default:
		printEvent, onSubagent, flush := chat.Events(o.Stdout, o.ToolOutput)
		printEvent = o.logprobTee(printEvent, func(engine.Event) error { return nil })
		judgePrint, _ := chat.JudgeEvents(o.Stdout, o.ToolOutput)
		return printEvent, onSubagent, judgePrint, nil, flush, nil
	}
}

// printJudges prints the per-judge judgement blocks for the chat and
// plain formats: a >>> Judge heading per judge in the chat heading
// style, then its judgement text, indented by two spaces per judge-chain
// depth. The judgements print from the outcomes after the judge chain
// completes; the ndjson format streams judge events live instead and
// prints no blocks.
func printJudges(w io.Writer, outcomes []engine.JudgeOutcome) {
	for _, o := range outcomes {
		ind := strings.Repeat("  ", o.Depth)
		fmt.Fprintf(w, "\n%s>>> Judge: %s\n\n%s\n\n", ind, o.Judge, indentBlock(o.Output, ind))
	}
}

// indentBlock prefixes every line of s, including the first, with the
// given indent.
func indentBlock(s, ind string) string {
	return ind + strings.ReplaceAll(s, "\n", "\n"+ind)
}

// logprobTee wraps a chat-style event printer with the logprob block
// behavior: after a whole assistant message event, when Logprobs is on and
// the event carries logprobs, one line prints per token after the response
// body (to stdout for plain — it is part of the run's output; alongside
// the body for chat). The write callback emits the event's text for the
// wrapped format's stdout handling; chat passes a no-op.
func (o Options) logprobTee(printEvent func(engine.Event) error, writeText func(engine.Event) error) func(engine.Event) error {
	return func(ev engine.Event) error {
		if err := printEvent(ev); err != nil {
			return err
		}
		switch ev.Kind {
		case engine.EventAssistantText:
			if err := writeText(ev); err != nil {
				return err
			}
			if o.Logprobs && len(ev.Logprobs) > 0 {
				printLogprobs(o.Stdout, ev.Logprobs)
			}
		case engine.EventAssistantTextDelta:
			if err := writeText(ev); err != nil {
				return err
			}
		}
		return nil
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
	return fmt.Errorf("unknown format %q (supported: %s)", o.Format, supported)
}
