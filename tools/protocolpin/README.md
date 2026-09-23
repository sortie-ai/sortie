# protocolpin

protocolpin compares the pinned Agent Client Protocol schema against the
publisher's GitHub releases and decides whether a report issue should open,
update, reopen, close, or stay as it is. It is built only for CI tooling and
is not included in the Sortie binary.

`protocolpin locate -repo-root DIR` reads the recorded pin from a repository
checkout and prints its repository, tag, and the report's title as one JSON
object.

`protocolpin decide -repo-root DIR` reads a bundle of the publisher's
releases, tag refs, tag object, and the report issue's current state from
standard input, and writes the chosen action and its rendered text as one
JSON object to standard output.

The command never writes a file and never moves the pin; `scripts/protocol-pin.sh`
gathers the input with `gh` and carries out the decision.

The report opens when the publisher's schema has moved ahead of the pin,
closes itself the first time a run finds no difference at all, and reopens
if the difference returns after a person closed it. Its index is its exact
title among this repository's issues labeled `area:agent-adapter`; renaming
it or removing that label starts a new report on the next run.
