package chat

import (
	"fmt"
	"io"

	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/tools"
	"github.com/overspecific/blorb/internal/usage"
)

// Usage accounting reaches the display over two event paths, and both are
// wrapped here:
//
//   - parent events come straight from the session engine via RunTurn's
//     onEvent callback — EventUsage events record the parent's calls;
//   - subagent events come from the registry's pipe (WithSubagentEvents)
//     as SubagentUsage events, one per completed LLM call by any agent in
//     the nested run.
//
// A usage event records a call that has already completed, so recording
// it never alters rendering: every event is forwarded unchanged and the
// usage event itself renders nothing.

// usageWrap composes a display callback with usage recording: every event
// is forwarded to print unchanged, and EventUsage events are additionally
// recorded into account with the event's attribution fields.
func usageWrap(print func(engine.Event) error, account *usage.Account) func(engine.Event) error {
	return func(ev engine.Event) error {
		if ev.Kind == engine.EventUsage {
			account.Add(usage.Record{Agent: ev.AgentName, Model: ev.Model, Usage: ev.Usage})
		}
		if print == nil {
			return nil
		}
		return print(ev)
	}
}

// subagentUsageWrap composes the subagent event callback with usage
// recording: SubagentUsage events are recorded into account (Agent already
// names the deepest producing agent) and every event is forwarded to print
// unchanged.
func subagentUsageWrap(print func(tools.SubagentEvent) error, account *usage.Account) func(tools.SubagentEvent) error {
	return func(ev tools.SubagentEvent) error {
		if ev.Kind == tools.SubagentUsage {
			account.Add(usage.Record{Agent: ev.Agent, Model: ev.Model, Usage: ev.Usage})
		}
		if print == nil {
			return nil
		}
		return print(ev)
	}
}

// printTurnFooter prints the per-turn usage footer when the turn made at
// least one LLM call, then folds the turn's records into the session
// account. A turn that errored after completing calls still prints its
// footer: the tokens were spent. The footer prints before any outcome
// line ("(interrupted)" / "error: ..."), so a partial turn shows its
// partial usage first.
func printTurnFooter(out io.Writer, turnAccount, sessAccount *usage.Account) {
	records := turnAccount.Records()
	if len(records) == 0 {
		return
	}
	fmt.Fprintf(out, "%s\n", usage.FormatTurn(turnAccount))
	for _, rec := range records {
		sessAccount.Add(rec)
	}
}
