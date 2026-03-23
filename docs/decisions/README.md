# Architecture Decision Records

This directory contains architecturally significant decisions for Sortie, documented as
[Markdown Architectural Decision Records (MADR)](https://adr.github.io/madr/).

## Decisions

| ADR                                              | Title                                                | Status   |
| ------------------------------------------------ | ---------------------------------------------------- | -------- |
| [0001](0001-use-go-as-core-runtime.md)           | Use Go as core runtime                               | Accepted |
| [0002](0002-use-sqlite-for-persistence.md)       | Use SQLite for persistence                           | Accepted |
| [0003](0003-adapter-based-integration.md)        | Use adapter interfaces for integration extensibility | Accepted |
| [0004](0004-workflow-file-format.md)             | Use YAML Front Matter for Workflow Files             | Accepted |
| [0005](0005-prompt-template-engine.md)           | Use Go text/template for Prompt Rendering            | Accepted |
| [0006](0006-use-fsnotify-for-file-watching.md)   | Use fsnotify for Filesystem Event Watching           | Accepted |
| [0007](0007-handoff-state-and-tracker-writes.md) | Handoff State and Tracker Write Contract             | Accepted |
