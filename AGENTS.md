We're building Blorb, a single-binary tool for making AI agents. It lets a user define an agent via a `blorb.json` file and then run them in a variety of ways - in a simple chat interface, one turn at a time via the CLI, or via HTTP. It lets users experiment with different tool setups and system prompts. Tools can be setup via the configuration file, and can be local scripts, subagents and more.

We use mise to setup and manage the development environment.

It is built in Go, with as few dependencies as possible. Everything has unit tests, and we aim for comprehensive test coverage. We are currently using Go 1.2.7 and we use the latest features where we can; we like generics. Errors are always checked and handled - the only exception being best-effort logging where it wouldn't make sense to stop operation due to an IO failure.

We work in git, and we only commit or push when the user explicitly asks.

We check our work using `bin/qc` - this formats, checks style and runs tests. We make sure this is passing before continuing to the next step.

NOTE: the word is "overspecific" - make sure you get it correct in paths.

## Markdown files
When writing Markdown, we do not manually wrap.