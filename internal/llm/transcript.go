package llm

import "strings"

// transcriptBodyIndent is the two-space prefix put on every body line of
// a transcript block. Because labels sit at column 0, embedded content
// (user text, tool output, file contents) cannot forge a block label:
// whatever it contains, it renders indented.
const transcriptBodyIndent = "  "

// FormatTranscript renders messages as a readable transcript: one
// block per message, labeled by role, in order, with every body line
// indented two spaces so embedded content cannot forge block labels.
// It is the text handed to a judge agent as its first user message:
// the full record of the judged agent's execution, including
// reasoning, tool calls, and tool results (failed ones carry the
// failure), so the judge can review the whole run. It is a display
// rendering, not a wire format; the messages' own JSON tags are for
// serialization, this is for reading.
func FormatTranscript(messages []Message) string {
	rendered := make([]string, 0, len(messages))
	for _, msg := range messages {
		if s := renderTranscriptMessage(msg); s != "" {
			rendered = append(rendered, s)
		}
	}
	return strings.Join(rendered, "\n\n")
}

// renderTranscriptMessage renders one message as its labeled blocks,
// joined by single newlines: the blocks of one message read as one
// unit. Returns the empty string when the message contributes nothing.
func renderTranscriptMessage(msg Message) string {
	var blocks []string

	switch msg.Role {
	case RoleTool:
		blocks = append(blocks, "[tool result]",
			transcriptBodyIndent+"id: "+msg.ToolCallID,
			indentTranscriptBody(msg.Content))
	default:
		if msg.Reasoning != "" && msg.Role == RoleAssistant {
			blocks = append(blocks, "[assistant thinking]",
				indentTranscriptBody(msg.Reasoning))
		}
		if msg.Content != "" {
			blocks = append(blocks, "["+string(msg.Role)+"]",
				indentTranscriptBody(msg.Content))
		}
		for _, tc := range msg.ToolCalls {
			blocks = append(blocks, "[tool call]",
				transcriptBodyIndent+"name: "+tc.FunctionName,
				transcriptBodyIndent+"arguments: "+tc.FunctionArgs)
		}
	}

	return strings.Join(blocks, "\n")
}

// indentTranscriptBody prefixes every line of s with the body indent,
// so a multi-line body stays indented on every line.
func indentTranscriptBody(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = transcriptBodyIndent + line
	}
	return strings.Join(lines, "\n")
}
