---
status: accepted
date: 2026-11-01
decision-makers: Serghei Iakovlev
---

# Hand the Agent Every Unseen Issue Comment Through One Shared Feed

## Context and Problem Statement

A person steers a Sortie agent by commenting on its issue. Whether a comment reached the agent depended on how the run began. A run the poll tick started carried no comments. A run a retry timer started carried every comment on its first turn. The first run after a park release carried, on its first turn, the comments created since the park. Continuation turns never read comments, so a correction written during a run waited for a later run, and a template without a comment variable lost comments silently. The park window kept Sortie's own stop comment, because the park time was stamped before that comment was posted, so the agent read the stop statement twice; the same window dropped comments written during the parked run's last turn. Nothing recorded which issue comments an agent had been given.

Comments will also come from sources other than the tracker (a dashboard chat, a terminal, automation relaying messages through an MCP tool) and leave through channels other than the next prompt, such as input into a running turn. Each pairing of source and situation must not become a mechanism of its own.

## Decision Drivers

1. **One rule at every prompt boundary.** Every prompt Sortie sends carries the comments its conversation has not been given, whatever started the run.
2. **Given once per conversation, never lost.** A comment the conversation holds is not sent again, and no restart, failed run, or crash drops one.
3. **Only comments the agent should act on.** Sortie's own comments never come back; who else counts is the operator's choice, with a narrow default.
4. **Placement stays with the operator, without silent loss.**
5. **Nothing moves while the feature is off.** Tracker call volume is unchanged, and no prompt changes except where the park window was wrong.
6. **Sources and channels are additive.** A new source or channel plugs into the same record and selection.

## Considered Options

- One persisted comment feed served at every prompt boundary, with placement decided by render comparison
- Per-situation comment handling (the status quo)
- A time watermark as the test for "given"
- Deduplication in memory
- A given-record of comment IDs without a body hash
- A given-record per issue instead of per conversation
- Freezing a comment's text at delivery and ignoring edits
- Recognizing echoes by author identity
- Forcing the comment block into every prompt
- Leaving placement to the template alone
- Letting the agent pull comments through its tracker tool
- An orchestrator-level watcher that reads comments for running issues

## Decision Outcome

Chosen option: **one persisted comment feed served at every prompt boundary, with placement decided by render comparison**, because it is the only option that delivers each comment once per conversation at every boundary, survives restarts, and keeps the operator's placement without losing comments.

**Boundaries.** A boundary is the first turn of any run (poll tick, retry timer, park release, resume after a budget hold or an operator pause, reopen, reaction), every continuation turn, and every self-review review and fix turn. Self-review turns run in the same conversation and a correction during verification is the same signal, so Sortie appends the feed's built-in block to its review and fix prompts; they have no operator template to compare. The worker reads the feed before rendering and reports what was delivered to the orchestrator, which stays the only writer of the record.

**Sources and channels.** A source yields comments with its name, a stable comment identity, the author's identity and kind, the body, and the creation time. The tracker is the first source. A push source persists each comment before accepting it, keyed by issue, source, and comment identity: an identical resubmission changes nothing, and a changed body replaces the stored one, which the edit rule then delivers. The comment stays pending until a delivery is recorded. A channel takes the pending comments and reports them delivered or refused. The next prompt works for every agent kind; a refused delivery falls back to it. A comment counts as given only through the channel that reported delivering it.

**Given record.** Sortie persists one row per issue, conversation, source, and comment identity, holding the SHA-256 of the normalized body, the delivering channel, and the time. A conversation is an agent session. Each agent adapter reports at session start whether the session carried its predecessor's history; one that did not, including a resume the runtime silently replaced with a new session, is a new conversation. The pending comments of a conversation are those in author scope, minus Sortie's own, minus those the conversation holds with the same hash. A new conversation also receives, as a separate group, the comments earlier conversations on the issue were given; its first boundary reads the issue's full comment list, because the record holds no comment text and those comments can lie outside an incremental read. Sortie records per turn once the turn has started with the prompt, whatever the turn's outcome, and only when the comments changed the rendered prompt. A crash between sending and recording re-delivers once and loses nothing. The existing record of review comments handed to reaction runs is not reused: it was written only when a run exited normally, so a run that sent its prompt and then failed recorded nothing; its rows carry no hash, channel, or conversation; and the review reactions depend on its exit-time meaning.

**Edits and deletions.** A comment whose hash differs from the recorded one is delivered once more, in full, marked as replacing the version the conversation holds; a new conversation receives only the current text. Deletions are ignored: a given comment cannot be withdrawn, and a retraction arrives as an edit or a new comment.

**Sortie's own comments.** `CommentIssue` returns the created comment's identity, and the tracker-comment notification route records it when it posts. Every comment Sortie writes is a notification event posted through that route, so the record covers stop reasons, lifecycle, budget-hold, and escalation notices. Authors listed in `comment_feed.exclude_authors` are dropped exactly, which is how an agent given its own tracker account is excluded. No other filter applies: no time window, text match, credential identity, or agent-written marker. Operators often run Sortie with a personal token, so "written by the credential's account" would drop their own comments. A comment the agent posts with its own tools under a shared identity is delivered: one repeated message in a continuing conversation, a report of earlier work in a new one.

**Author scope.** `comment_feed.authors: members`, the default, delivers comments from people with write access to the project plus authors listed in `comment_feed.include_authors`; `all` delivers every author. Bots and automations count only when listed. On a forge Sortie reads the author's access through a route the tracker token can already read: GitHub's collaborator-permission endpoint, GitLab's project membership, and on Gitea the repository's assignee list, because Gitea's permission endpoint answers only an administrator about another account. Sortie caches a granted answer for the life of the process, and caches a refusal per comment, so a later comment from someone granted access is checked again. On Jira and Linear commenting already requires project or workspace access, so `members` is every identified person there; Jira Service Management customers, Jira app accounts, Linear apps and bot actors, and comments created through integrations count only when listed, and a comment with no identifiable author is outside `members`.

**Placement.** Every render receives `comment_feed`, holding two disjoint views of the pending comments: `new` (comments no conversation on the issue was given, plus edits to comments this conversation holds, marked as replacing) and `earlier` (comments an earlier conversation was given, at their current text with any edit applied, filled only in a new conversation). Both are empty lists when nothing is pending or the feature is off. On a turn with pending comments Sortie renders the template with the feed views filled and emptied, the comparison ADR-0037 uses for reaction prompts. When the renders differ, the template placed the comments and is sent unchanged, and every pending comment counts as given. Otherwise Sortie appends its built-in block (author and time on each comment, oldest first, earlier comments apart from new ones) after the operator's render or the reaction prompt and before the text Sortie already appends. A template omits comments deliberately by rendering something else in their place, such as a line pointing the agent at its tracker tool; the other opt-out is turning the feature off. `.park.comments` becomes a view of the feed: with the feature on, the release run's pending comments; with it off, the comments created since the parked run's last prompt. Either way Sortie's own comments are removed, which ends the doubled stop statement and restores the comments written during the parked run's last turn. `.issue.comments` stays the raw list.

**Configuration and cost.** The top-level `comment_feed` block holds `enabled` (default `true`), `authors`, `include_authors`, and `exclude_authors`; a dispatch reads it when it starts. The feed is on by default because a comment steers the agent only if it arrives, and the narrow author scope keeps that default safe; an operator who leaves comments to the agent's own tools sets `enabled: false`. The deprecated `tracker.comments` map is unrelated. With the feature on, each boundary makes at most one comment read, and a first turn whose dispatch already read comments makes none; a read is one request per page of comments. Each tracker uses its cheapest read: GitHub a conditional request on a URL that stays stable while nothing changes, whose not-modified answer costs no rate limit; GitLab notes ordered by update time, newest first, read down to the first note updated before the newest update time the previous read saw, so notes that share that time are all read; Gitea a server-side `since` filter; Linear a server-side update-time filter; Jira newest-first pages with every comment on them compared by hash, fetching the next page only while the oldest comment on the current one is not yet in the record. The read boundary only limits what is fetched; identity and hash decide what is delivered. The issue's updated timestamp is not a change probe: GitLab skips the bump for a comment made within a minute of another update, and Jira's moves on Sortie's own writes.

### Considered Options in Detail

**Per-situation handling.** Each start path read comments its own way, continuation turns read none, and the park window both duplicated and dropped comments. A new source would add another path.

**Time watermark.** Tracker timestamps have whole-second resolution and filters disagree on inclusive bounds, so a watermark either repeats or skips a comment in the boundary second, and it cannot tell one conversation from another. Sortie uses update time only to narrow a read.

**Deduplication in memory.** A restart either repeats every comment or, with a watermark, loses those written while Sortie was down.

**IDs without a hash.** An ID set cannot see an edit, and a correction typed into an existing comment is the realistic case.

**Record per issue.** A fresh conversation, which holds nothing, would never see comments an earlier one received.

**Freezing text at delivery.** Hosted agents that freeze the text guard one-shot runs against tampering; in a long-lived conversation the same rule loses a person's correction made as an edit.

**Author-identity echo filter.** With a shared personal token, the credential's identity is also the operator's, so the filter drops the comments it exists to deliver.

**Forced injection.** Appending regardless of the template repeats comments a template already placed and takes placement from the operator.

**Template only.** A template without the variable loses comments silently. A static check for a mention of the variable cannot see that a template placing comments only on the first turn leaves later turns empty; ADR-0037 rejected a second model of template semantics for the same reason.

**Agent pulls through its tool.** The agent does not know a comment exists until it looks, a look costs a turn, the tool is registered only when `tracker.project` is set, and nothing records what it read.

**Orchestrator-level watcher.** Reads for running issues on a timer add calls outside each run's bound and cannot know when a prompt goes out; the worker reads at the boundary it is about to cross.

## Consequences

### Positive

- One selection and one record serve every boundary, every source, and every channel; mid-turn delivery becomes a second channel without changing the record.
- A restart, failed run, or crash never drops a comment.
- `.park.comments` stops repeating the stop statement and stops dropping last-turn comments.

### Negative

- Adapter contracts change: `CommentIssue` returns the created identity on every tracker; comments carry author identity and kind; each tracker gains its incremental read and each forge a permission lookup; agent adapters report whether a session carried history, which the Codex and Agent Client Protocol kinds do not do when a resume falls back to a new session.
- Upgrading changes prompts and tracker call volume for every workflow that does not set `comment_feed.enabled: false`.
- With the feature on, a run makes up to one extra comment read per turn: at the default `agent.max_turns` of 20, up to 19 beyond the first turn, plus one per self-review turn, a full comment read at the first boundary of each new conversation, one permission lookup per forge author not yet granted, and one more for each new comment from an author who was refused.
- The schema gains the given record and the own-comment record, with rows per conversation.
- `.park.comments` changes for existing workflows even with the feature off.
- A template that shows part of the feed marks all pending comments given.
- A template that renders `.issue.comments` and not the feed also receives the built-in block, so on turns where `.issue.comments` is filled a comment can appear twice until the template places `comment_feed`.
- On Jira an edit to a comment older than the comments a read reaches is not seen.
- A notice whose create response was lost has no recorded identity and reaches the agent once.
- An author whose forge access is revoked still counts until Sortie restarts, a Jira project that grants comment permission broadly widens `members` to match, and on a Gitea organization repository `members` also covers members of the repository's teams that can read but not write.
- The agent's own comments under a shared identity reach it again, and so do comments from an account of its own until that account is listed in `comment_feed.exclude_authors`.
