---
name: visioning
description: Use when the user wants to create, develop, or refine a vision document, project charter, or high-level conceptual overview for a system or feature.
---

# Vision document development

Help the user produce an actionable vision document through conversation, questions, and light suggestions.

## Goal

Produce a vision file that captures the user's intent at whatever level of detail they want. Also produce a conversation log file that records how the vision was developed.

## Rules

- Let the user drive the depth. The document can be sparse in some areas and detailed in others.
- Ask clarifying questions rather than fill in gaps yourself.
- Update the vision file after each clarifying interaction. Do not keep the document in memory.
- Assume the user has the vision file open in their editor. Do not paste the full document into the chat; instead, give a short summary of what was just added or changed.
- Maintain a conversation log file.
  - Each time the conversation shifts to a new topic, start a new section with a short heading and a timestamp.
  - If the conversation returns to a previous topic later, start another new section for it.
  - The log is append-heavy: add new sections rather than rewriting old ones.
- Offer structure. Ask if they'd like to organize by goals, constraints, stakeholders, scope, or risks.
- Suggest angles they might not have considered, but do not push them. Accept "I don't know yet" or "that doesn't matter."
- When the user signals they are done, confirm the vision file is the final document. No additional write is needed unless the user requests a different format or location.
- Write the document in plain prose, not rigid templates, unless the user asks for a specific format.
- Default file location: `.agents/visions/<kebab-case-topic>.md`.
- Default log file location: `.agents/visions/<kebab-case-topic>-log.md`.
- Capture open questions and unresolved areas at the end of the vision document.

## Workflow

1. **Name the files.** Confirm the topic and lock down the kebab-case base name for the vision file and conversation log file. If the user supplied one, use it; otherwise propose one.
2. **Check for existing files.** Look for an existing vision file or conversation log file. If found, read them before continuing and treat the session as an update.
3. **Open the conversation.** Ask what the vision is for, whether they have a draft or are starting from scratch, why it matters, and who it is for.
4. **Discuss.** Follow the user's lead. Ask clarifying questions about whatever aspect they want to explore next (goals, constraints, stakeholders, scope, risks, etc.).
5. **Summarize.** Give a short summary of what was just added or changed, then ask what to discuss next.
6. **Update the files.** Rewrite the vision file, then append a new section to the conversation log file covering the topic just discussed.
7. **Iterate.** Repeat steps 4-6 until the user is satisfied.
8. **Confirm.** Offer a final review. If the user agrees, the vision file on disk is the final document.

## Example openings

- "What is this vision for?"
- "Who is the audience for this vision?"
- "Do you have notes or a rough draft already, or are we starting blank?"
- "What would make this vision actionable for you?"
- "Are there areas you already know well, and others where you want help thinking it through?"
