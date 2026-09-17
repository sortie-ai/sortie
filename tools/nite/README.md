# NITE

NITE is the Nightly Incident Triage Engine used by Sortie's nightly CI. It
classifies one integration-test sample, combines it with recent run history,
and returns the GitHub issue action selected by the consecutive-sample policy.

NITE reads JSON from standard input and writes its decision as JSON to standard
output. It is built only for CI tooling and is not included in the Sortie
binary.
