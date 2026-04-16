---
status: accepted
date: 2026-04-16
decision-makers: Serghei Iakovlev
---

# Keep TrackerAdapter as a Unified Interface

## Context and Problem Statement

The `TrackerAdapter` interface has 9 methods: 6 read operations, 2 core write operations
(`TransitionIssue`, `CommentIssue`), and 1 escalation write operation (`AddLabel`). As
the roadmap anticipates additional tracker write methods (`RemoveLabel`, `AddAssignee`,
`ChangeStatus`), the question is whether the single-interface pattern will become a
maintenance burden that violates Go's small-interface convention, or whether splitting
introduces unnecessary complexity for the current scale.

## Decision Drivers

1. **Compile-time safety.** All adapter implementations are internal to this repository.
   The compiler should catch every missing method at build time, not at runtime via type
   assertions.
2. **Orchestrator simplicity.** The orchestrator calls tracker methods unconditionally.
   Introducing type-assertion branches adds conditional logic, fallback handling, and
   additional test paths at every call site.
3. **No-op cost.** When an adapter does not meaningfully support a method, it returns
   `nil`. The cost of one no-op stub per adapter per method is trivial at current scale.
4. **Go interface cohesion.** Interface size is justified when methods form a cohesive
   behavior contract. All 9 methods describe "what an issue tracker can do for the
   orchestrator" — a single responsibility.

## Considered Options

- Keep `TrackerAdapter` as a single unified interface
- Split into `TrackerAdapter` (core read/write) + `TrackerEscalation` (optional capability
  discovered via type assertion)

## Decision Outcome

Chosen option: **Keep `TrackerAdapter` as a single unified interface**, because the cost
of splitting exceeds the cost of the current pattern at the present scale.

Only 1 of 9 methods is a no-op in 1 adapter (`FileAdapter.AddLabel`). Splitting would
require type-assertion branches at 2 orchestrator call sites, trading compile-time safety
for runtime discovery — a trade that is unnecessary when all adapters are internal and
every implementation is under our control. The 10 test doubles each gain one stub line per
new method; saving approximately 3 stubs does not justify the structural change.

### Reassessment Trigger

Revisit this decision when **all three** conditions are true simultaneously:

1. The interface exceeds 12 methods.
2. At least 3 methods are no-ops in 2 or more adapter implementations.
3. A concrete new adapter is being built that genuinely cannot support the no-op methods
   (e.g., a read-only tracker where write methods would violate the adapter's contract
   rather than being harmless no-ops).

Until these conditions are met, the unified interface is the simpler and safer choice.

### Considered Options in Detail

**Split into core + optional escalation interface.** Under this option, escalation methods
(`AddLabel`, future `RemoveLabel`, `AddAssignee`) would move to a `TrackerEscalation`
interface. The orchestrator would discover the capability via type assertion
(`if esc, ok := tracker.(TrackerEscalation); ok { ... }`). This pattern is appropriate
when adapters are external and the set of implementations is open. For Sortie, all
adapters are internal, the set is closed, and the type-assertion branches add complexity
without benefit. Each call site gains a branch, a log line, and a test path for the
"not supported" case — producing the same outcome that today's no-op `nil` return achieves
with zero branching.
