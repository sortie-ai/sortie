---
status: accepted
date: 2026-10-30
decision-makers: Serghei Iakovlev
---

# Reset a Usage-Limit Refusal at the Latest Reset of Its Gating Windows

## Context and Problem Statement

When a provider refuses a turn because a usage allowance is exhausted and the refusal carries no reset of its own, Sortie reads the reset from the usage windows the turn-end pull reports. Several windows can be exhausted at once, and the refusal needs one instant at which its hold on new sessions ends.

The rule was the earliest future reset among the exhausted windows, so that a later reset belonging to another allowance would not hold a model too long. But provider windows are nested allowances on one account: a request passes only when every window that applies to it has room. With a five-hour and a weekly window both exhausted, the hold ended at the five-hour reset and the next session was refused by the weekly window. A hold covers a whole agent kind or model and held retries re-arm at its release, so one early wake could cost one refused session per held retry. The OpenCode console reports rolling, weekly, and monthly windows that name no model; the earliest rule woke about a month too early there.

## Decision Drivers

1. **Wake when a request can pass.** The hold ends when every window that applies to the refused request has room.
2. **An early wake is costly.** It spends a launch and a refusal per held retry.
3. **Another model's allowance does not gate this model.**
4. **No partial answer.** The ordinary retry backoff already covers an unknown reset.

## Considered Options

- Earliest reset among the exhausted windows
- Latest reset among the exhausted windows that gate the request
- Per-allowance grouping, taking the earliest reset that frees one allowance

## Decision Outcome

Chosen option: **latest reset among the exhausted gating windows**, because it alone ends the hold when the refused request can pass, and a model filter answers the concern behind the earliest rule.

- The gating windows are those that name no model, which meter the account, plus the configured model's windows. A window that meters another model does not count. With no model configured, the refused model is unknown and every window counts.
- The refusal's reset is the latest future reset among the exhausted gating windows.
- If an exhausted gating window has no future reset, the windows supply no reset and the refusal retries on the backoff.
- A reset the refusal or its records carry outranks every window.

Where a provider names one overall reset (Anthropic's unified reset, the window Codex selects, Gemini's retry delay), it is the binding window. The IETF RateLimit header draft, Anthropic's API, and client-go's max-of rate limiter each combine several limits by the most restrictive one.

### Considered Options in Detail

**Earliest reset.** Rejected on drivers 1 and 2. It is right only when windows are alternatives, and it converges after one wasted wake per intermediate window.

**Per-allowance grouping.** Rejected on driver 4. No pulled window states which allowance it belongs to or whether allowances are alternatives, so the grouping would be guessed, and a wrong guess reproduces the early wake.

## Consequences

### Positive

- A held kind or model wakes once, at the reset of the window that binds it.

### Negative

- A runtime that treats windows as alternatives, switching to a model with its own allowance when one runs out, is held longer than needed. Without a configured model this can happen, because every window counts.
- Capacity restored out of band, such as bought credits or a plan upgrade, is not seen until the reset, as with a single window.
