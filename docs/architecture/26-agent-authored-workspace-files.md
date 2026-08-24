## 21. Agent-Authored Workspace Files

Agents may create files in the workspace that Sortie reads for orchestration decisions. This is an
optional extension that enables richer agent-to-orchestrator communication beyond the event stream.

These files are the filesystem channel from the agent to the orchestrator: the agent writes,
Sortie reads after the turn, and the signal changes orchestrator control flow (retry suppression,
handoff, the review loop). They are distinct from the `notify_operator` tool (Section 10.4.5),
which reaches a human operator on a configured channel in real time during the turn and changes
no orchestrator state. An agent that wants to influence scheduling writes one of the files below;
an agent that wants to inform a person calls `notify_operator`. Section 10.4.6 contrasts the two
channels in full.

### 21.1 `.sortie/status`

An agent may write a `.sortie/status` file in the workspace root to signal progress or request
specific orchestrator behavior. Sortie reads this file after each turn completes.

Recognized values:

- `blocked`: agent signals it cannot proceed without human intervention. The orchestrator treats
  this as a soft stop: it completes the current turn normally, suppresses continuation retries,
  and releases the issue claim. Where the dispatch drives issue state, it also parks the issue: it
  records a durable park and applies the parking label, and holds the issue out of dispatch until
  it observes a change in the issue's tracker state or the removal of a parking label it has
  confirmed reached the tracker (Section 14.2). Read after a coding turn, this value never enters
  the self-review phase (Section 7); it remains an immediate exit whether or not self-review is
  configured. Written during a phase turn instead, `blocked` ends the phase and gives the run this
  same disposition, distinguished from the coding-turn read by the phase's own tail work: the
  review-summary write and the non-`disabled` `SORTIE_SELF_REVIEW_STATUS` value the `after_run`
  hook then receives (Section 13.8).
- `needs-human-review`: agent signals that work is complete and requires review. Like `blocked`,
  this value suppresses continuation retries and releases the issue claim. Unlike `blocked`, when
  `tracker.handoff_state` is configured, the issue is in an active tracker state, and the dispatch
  drives issue state, the orchestrator performs the handoff transition (Section 5.3.1). This
  ensures completed work moves
  to a review state in the tracker, maintaining tracker-as-source-of-truth semantics. If
  `tracker.handoff_state` is not configured, the issue becomes eligible for re-dispatch on future
  tracker polls under normal dispatch rules; unlike `blocked` in that deployment, no park is
  recorded, so the two values are still distinguishable. The transition is also subject to the
  run's `tracker.handoff_evidence` verdict (Section 7.3): a verdict that withholds it is checked
  once more against the tracker before the exit records it. Only when that check does not find the
  issue terminal does the withheld verdict leave the issue in its active state, keep the claim, and
  take the backoff failure path instead of releasing the claim; when it does find the issue
  terminal, the exit releases the claim instead. Where
  self-review is enabled and the phase's other gate conditions hold, this value first admits the
  run to the self-review phase (Section 7); the handoff transition and claim release described
  above happen once that phase ends, rather than at the read.

If the file is absent or contains an unrecognized value, it is ignored.

The `.sortie/status` file is not required for any core orchestration behavior. It is an advisory
channel only.

The full protocol specification, including file format, parsing rules, read timing, cleanup
obligations, versioning, security considerations, and design rationale, is in
[agent-to-orchestrator-protocol.md](../agent-to-orchestrator-protocol.md).

### 21.2 `.sortie/review_verdict.json`

During the self-review phase, the agent writes a structured review verdict to
`.sortie/review_verdict.json`. The orchestrator reads this file after each review turn to
determine the next action.

JSON schema:

```json
{
  "verdict": "pass | iterate",
  "issues": [
    {
      "file": "path/to/file.go",
      "line": 42,
      "severity": "error | warning | info",
      "message": "Description of the issue"
    }
  ]
}
```

- `verdict` (string): `"pass"` ends the review loop; `"iterate"` requests a fix turn.
  Any other value is rejected as invalid.
- `issues` (array, optional): structured list of review findings for the fix prompt.

Safety rules:

- Maximum file size: 65536 bytes (64 KB). Oversized files are rejected.
- Symlink protection: both `.sortie/` and `.sortie/review_verdict.json` are checked via
  `Lstat` before reading. If either is a symbolic link, the file is rejected. This follows
  the same pattern as `.sortie/status` (Section 21.1).
- Missing or invalid verdict content on a non-final iteration is treated as `"iterate"`.
  Missing or invalid verdict content on the final iteration does not count as `"pass"`; the
  run ends with no final verdict recorded and `CapReached=true`.

