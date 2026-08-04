---
status: proposed
date: 2026-08-04
decision-makers:
---

# Place Forge Integrations in One Package per Forge

## Context and Problem Statement

Sortie integrates with two kinds of external product. A **tracker-only** product supplies
issues and nothing else. A **forge** supplies issues, code review, and continuous
integration in one system, so a single integration ends up implementing three separate
adapter contracts: the tracker contract, the source-control contract, and the CI status
contract.

The adapter tree grew to reflect the first kind before the second existed. Tracker-only
integrations live one package per tracker under `internal/tracker/`. Forge integrations live
one package per forge under `internal/scm/`, each holding its tracker, source-control, and
CI implementations together. Both families register their implementations by kind through
`internal/registry`.

That leaves an unresolved question whenever a new forge integration begins, because a forge
integration is almost always built in stages and the tracker half comes first. At the moment
the first line is written, the package contains only a tracker, which makes it look like a
member of the tracker family; by the time the integration is complete it holds three adapter
roles, which makes it a member of the forge family. Choosing the tracker family for the sake
of the first stage buys consistency with the tracker-only integrations and pays for it with a
package move later. Choosing the forge family up front means a package whose name promises
source-control support that the first stage does not yet provide.

A concrete instance forces the question: GitLab is a forge whose API is not compatible with
any existing integration, so it needs a native package, and its issue surface is the natural
first stage.

## Decision Drivers

1. **A forge's adapter roles share wire machinery, not domain logic.** Within one forge, the
   issue surface and the code-review surface use the same authentication scheme, the same
   project addressing rules, the same pagination mechanism, the same error envelopes, and the
   same rate-limit signalling. In several forges the comment entity and the label-event
   journal are literally the same resource reached under a different parent path. Splitting
   those roles across packages would either duplicate that knowledge or require a third
   package to hold it.
2. **Cross-forge sharing has no such basis.** Two different forges agree on almost nothing at
   the wire level even when their domain concepts look alike. The overlap that does exist is
   generic HTTP behavior, which already lives in the shared transport helpers.
3. **A package move is pure churn.** Relocating a package rewrites every import, moves every
   test file, and touches registration, for no change in behavior. The cost is certain and
   the benefit is zero, so a layout that avoids the move is worth a small amount of initial
   awkwardness.
4. **Placement is invisible to callers.** The orchestrator and the command layer resolve
   adapters through `internal/registry` by kind and never import an integration package
   directly. Directory placement is therefore an internal organization question, not a
   contract with the rest of the system.
5. **Boundary rules must not vary by family.** Both families already forbid cross-adapter
   imports and imports of orchestrator packages, and both require external responses to be
   normalized to domain types at the package boundary. A decision about placement must not
   create a family with weaker rules.

## Considered Options

- One package per forge, under the source-control adapter family, holding every adapter role
  that forge supports
- One package per adapter role, placing a forge's tracker under the tracker family and its
  source-control and CI roles elsewhere
- One package per forge, plus shared base types unifying the equivalent roles across
  different forges

## Decision Outcome

Chosen option: **one package per forge, under the source-control adapter family, holding
every adapter role that forge supports**, because it is the only option that keeps a single
product's wire knowledge in one place while avoiding a later package move, and it does so
without inventing cross-forge abstractions that the wire formats do not support.

A forge integration is therefore created under `internal/scm/<forge>` from its first stage,
even when that stage implements only the tracker contract. The package registers each adapter
role under the forge's kind as that role is implemented. `internal/tracker/` remains the
correct home for tracker-only integrations, which have no second or third role to grow into;
this decision does not migrate them and does not deprecate that family.

Sharing between packages stays narrow and deliberate. A forge package depends on the shared
transport, issue-normalization, metrics, and configuration-decoding helpers, and on nothing
else outside the domain types. It does not import a sibling integration package, and it does
not share base structs with one. Where two integrations genuinely need the same behavior, the
behavior belongs in a shared helper package rather than in a base type that couples two
independently versioned external APIs.

The naming mismatch in the first stage is real and is accepted as the lesser cost. A package
under the source-control family that currently implements only a tracker is mildly
misleading to a reader browsing the tree; a package that must be moved once the second role
arrives is misleading in the same way for the same period and additionally imposes the
rewrite. The mismatch also resolves itself, because the whole reason to choose this layout is
that the remaining roles are expected.

### Considered Options in Detail

**One package per adapter role.** Under this option a forge's tracker would sit beside the
tracker-only integrations and its source-control and CI roles would sit under the
source-control family. This is the most consistent choice if the tree is read as a
classification of adapter roles, and it is the wrong choice because the tree is really a
classification of external systems: everything a package needs to know is a fact about one
external product's API, and that product does not respect the role boundary. Concretely, the
authentication and project-addressing code would have to exist in both packages or in a third
one, and a change in the forge's error envelope would require edits in two places that must
stay in agreement. It also produces two packages registering the same kind, which makes the
registry's kind namespace ambiguous about where an integration lives.

**One package per forge, plus shared base types across forges.** Under this option the
equivalent roles in different forges would share base structs, on the theory that all forges
expose issues, reviews, and pipelines. The theory does not survive contact with the wire
formats. Forges disagree on identifier semantics, on whether a workflow state exists at all,
on label mutability, on pagination style, on which operations are available without a paid
licence, and on the shape of an error. A base type spanning them would accumulate optional
fields and capability flags until every subclass overrode most of it, and each new forge
would force edits to a type that the existing forges depend on. The narrow-helper approach
gets the real reuse, because the genuinely common part is HTTP transport and pagination
mechanics, which is exactly what the shared helpers already provide.

## Consequences

### Positive

- A forge integration never needs to move, so the cost of starting with only the tracker role
  is limited to the first stage's naming mismatch.
- All of one external product's wire knowledge sits in one package, so a change in that
  product's authentication, pagination, or error format is a single-package edit.
- The registry's kind namespace stays unambiguous: one kind, one package.
- Tracker-only integrations are unaffected, so the decision adds no migration work.

### Negative

- A package under the source-control family may implement only a tracker for some time, which
  is not obvious from its path and must be discoverable from the package documentation
  instead.
- The source-control family becomes the larger and busier of the two adapter families, and
  its packages are correspondingly larger, since each holds three adapter roles.
- The rule requires judgement for a product that is not clearly one kind or the other. A
  product supplying issues plus code review but no CI, or one whose code-review surface Sortie
  has no intention of ever using, must be classified deliberately rather than mechanically.
