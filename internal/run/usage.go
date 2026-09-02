package run

import (
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/tools"
	"github.com/overspecific/blorb/internal/usage"
)

// runUsageWrap composes the subagent event callback with usage recording,
// mirroring chat's subagentUsageWrap: run deliberately replicates chat's
// small helpers rather than sharing unexported internals (the same
// reasoning as newClient and isTerminated). SubagentUsage events are
// recorded into account and every event is forwarded unchanged.
func runUsageWrap(print func(tools.SubagentEvent) error, account *usage.Account) func(tools.SubagentEvent) error {
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

// usageWrap composes the engine event callback with usage recording,
// mirroring chat's usageWrap: every event is forwarded to print
// unchanged, and EventUsage events are additionally recorded into
// account with the event's attribution fields.
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
