---
status: accepted
date: 2026-11-11
decision-makers: Serghei Iakovlev
---

# Keep Stable Routes of the JSON API Backward Compatible Within `v1`

## Context and Problem Statement

The JSON API under `/api/v1/` serves the embedded dashboard and is also built on by software the project does not ship: scripts, aggregators over several instances, and alternative operator interfaces. The API is described by `docs/openapi.json`, an OpenAPI 3.2 document that a test generates from the Go response types and a route table.

Before this decision the project made no statement about compatibility. A client could not tell whether an upgrade might rename or drop a response member, and nothing in the project prevented it: the description was regenerated whenever a response type changed, so a removed member passed every check once the generated file was refreshed. Clients compared descriptions between releases by hand.

The project is young. The API will keep changing, and an incompatible change will occasionally be necessary. What may a client of `/api/v1/` rely on across releases, and by which paths may the project still make an incompatible change?

## Decision Drivers

1. **A contract a client can act on.** A client must be able to tell, from published material alone, which parts of the API it may build on.
2. **Warning before removal.** Something a client relies on does not disappear without notice the client could have seen.
3. **Room to change.** The policy must not freeze the API or make a necessary incompatible change expensive for a young project.
4. **No silent break.** Every way to make an incompatible change is explicit and visible in review. Regenerating a file is never one of them.
5. **Standard mechanisms.** Each mechanism is a widely used one that needs no hand maintenance. The project writes no comparison logic of its own.
6. **One origin.** The Go response types remain the single origin of every schema.

## Considered Options

- Promise additive compatibility for stable routes, with explicit paths for an incompatible change, enforced by comparing the description with the latest release
- Declare the whole API unstable until a later date
- Freeze `v1` and move every incompatible change to a new path version
- Publish a prose promise without a mechanical check

## Decision Outcome

Chosen option: **Promise additive compatibility for stable routes, with explicit paths for an incompatible change, enforced by comparing the description with the latest release**, because it is the only option that gives clients a contract today (drivers 1 and 2), keeps incompatible changes possible and cheap (driver 3), and makes each of them a reviewed act (driver 4).

### The contract

The contract of a release is `docs/openapi.json` as committed at that release's tag. Every release attaches that file to its GitHub release as an asset named `openapi.json`, with no version in the name. The description of the newest release is therefore at `https://github.com/sortie-ai/sortie/releases/latest/download/openapi.json` and that of any release at `https://github.com/sortie-ai/sortie/releases/download/<tag>/openapi.json`, which is how a client pins a version and compares two.

The description covers every route under `/api/v1/` and the health routes `/livez` and `/readyz`: methods, path, query, and header parameters, status codes, response bodies, and the error envelope. Events of a `text/event-stream` response are described event by event, so event names and payload schemas are part of the contract exactly as ordinary response bodies are.

The promise extends as far as the description and no further. Where the description leaves something open, for example a payload described only as an object, nothing beyond that is promised.

The policy takes effect with the first release whose tag carries the description.

### Stability levels

A route is either **stable** or **not yet stable**. The level belongs to the route and method as a whole, never to a single member.

- Every route in the description when the policy takes effect is stable.
- A new route may ship as not yet stable. The description marks it with `x-stability-level`. It may change or disappear in any release without notice.
- A route without the mark is stable. A not yet stable route is promoted by removing the mark.
- A stable route is never demoted.

### Compatibility on stable routes

Within `v1`, a stable route is only added to. The following do not happen except through deprecation or an acknowledged break:

| Element | Incompatible change |
| --- | --- |
| Route or method | Removal |
| Request parameter | Removal; a new required parameter; an optional parameter becoming required; a change of type; removal of an accepted value |
| Success status code | Removal or replacement |
| Response member, including a member of a stream event payload | Removal; a change of type; becoming nullable; becoming optional |
| Stream event name | Removal or renaming |
| Error envelope | Any change to its shape |
| Error code | Renaming, or reuse with a different meaning |
| Any of the above | A change of meaning under an unchanged name and type |

The following are compatible and may appear in any release:

- a new route;
- a new optional request parameter, or a new accepted value of an existing one;
- a new response member;
- a new value of an enumerated response member;
- a new stream event name;
- a new error code, or an additional error status on a route, carried in the same error envelope.

An addition to a stable route is covered by the promise from the first release that contains it.

A client must therefore ignore response members, enumerated values, and stream event names it does not know, and must treat an unknown error code according to its HTTP status.

### Deprecation

A route, a request parameter, or a response member that is to be removed is first marked `deprecated` in the description. The changelog entry of the release that first carries the mark names the element and what replaces it. The element stays, unchanged in behavior, for at least 60 days after that release. Removal may then ship in any release, including a minor one, and is listed in that release's changelog.

### Acknowledged break

An incompatible change that cannot wait for the notice period is reserved for a security or correctness defect in the contract itself. The change adds one line per incompatible difference to an acknowledgement file in the repository, and the changelog of the release that ships it announces the break. Convenience, cleanup, and renaming are not grounds for this path; they go through deprecation.

### Outside the promise

- Any route marked not yet stable.
- Human-readable text: status sentences, summaries, and other members documented as display text, and the wording of error messages. Their presence and type are covered; their content is not. A client branches on error codes and enumerated members, never on text.
- The order of members in an object.
- `/metrics`, the dashboard under `/ui/`, the root page, and the favicon. None of them is in the description.
- Changes between releases on the development branch. Only released descriptions are compared.

### Path version

`v1` in the path changes only for a redesign that cannot be expressed through additions and deprecations. That is what `/api/v2/` would be for. No such version is planned, and this record sets no rules for one. Product releases do not signal API compatibility through their version number; the description and the changelog do.

### Mechanism

The policy rests on tooling chosen separately and stated here only as far as the policy depends on it.

- A test in the server package generates the description from the Go response types and a route table, using the standard library only. It fails when a registered route is missing from the table, so the description cannot drift from the server.
- The pull-request workflow runs oasdiff, a standalone tool at a pinned version that is never a Go module dependency. It validates the description and compares it with the copy at the latest release tag in the repository, failing on an incompatible difference. The check does not download the release asset.
- The comparison leaves out routes marked `x-stability-level`, accepts an element becoming `deprecated`, accepts removal of an element already deprecated at the latest release tag, and accepts a difference named in the acknowledgement file while still failing on any other.
- The docs site renders the route and schema reference from the `openapi.json` asset of the latest release, never from the product's source tree. The compatibility statement for clients is prose beside it, written from this record.

### Relation to earlier records

`0034-build-the-dashboard-as-an-embedded-react-application.md` introduced the generated type contract as a guard that keeps the dashboard's TypeScript declarations equal to the Go response types. This record does not change that guard. It gives the generated description a second role, as the published contract for clients outside the project, and attaches a compatibility policy to it that the earlier record did not have.

### Considered Options in Detail

**Declare the whole API unstable until a later date.** It costs the project nothing and fails drivers 1 and 2 completely: a client learns only that it may rely on nothing. The record of past releases, which added members and removed none, shows the project can afford a stronger promise.

**Freeze `v1` and move every incompatible change to a new path version.** The promise is the simplest to state, but each small correction would require a parallel route tree, served and tested alongside the old one for as long as clients remain on it. That cost falls hardest on a young API and contradicts driver 3. Tying breaks to the product's major version has the same effect, since it holds every correction back until a release that may be far away.

**Publish a prose promise without a mechanical check.** A promise kept only by attention fails driver 4. The defect it is meant to prevent, a member removed by regenerating a file, is exactly the one a reviewer does not see in a large generated diff.

## Consequences

### Positive

- A client can decide from the description alone what it may build on, and can compare any two releases mechanically.
- An incompatible change to a stable route cannot merge unnoticed.
- New work can ship as not yet stable and settle before it is promised.

### Negative

- Every route present when the policy takes effect is promised at once, including recently added ones whose shape may still prove wrong. Correcting one costs a deprecation cycle.
- A deprecated element is carried, served, and tested for at least 60 days.
- The comparison sees structure only. A change of meaning, a renamed error code, a new stream event's effect on a strict client, and the length of the notice period are held by review.
- Clients carry a duty of tolerance. One that rejects unknown members or values breaks on a compatible release.

## Confirmation

- A change that removes a response member or a route from a stable route fails the pull-request check and names the element.
- A change that adds a route, an optional parameter, or a response member passes.
- A change to a route marked not yet stable passes.
- Removing an element already marked deprecated at the latest release tag passes; removing one that is not fails.
- With an acknowledgement line in place, the named difference passes while a second, unacknowledged one in the same change still fails.
- Review checks what the comparison cannot: that a removal comes at least 60 days after the release that first carried the mark, that the changelog announces each deprecation, removal, and acknowledged break, and that no member changes meaning under an unchanged name.
