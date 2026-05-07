# Context

Domain glossary for the release-automation reconciler. Add new terms here
when a refactor names a concept the codebase reasons about but hasn't
written down.

## Terms

**Issue state envelope**
The fenced YAML metadata block embedded in a GitHub issue body that holds
state surviving between reconciler ticks. Has a versioned open marker
(e.g. `<!-- bump-op-state v1`) and a close marker (`-->`). Each tracker
flavor (bump-op, cascade) defines its own marker and its own `Persistent`
type; the envelope mechanism — fence parsing, YAML round-trip, replace-
or-append on write — is shared in `internal/issuestate`. The envelope is
strictly the *transport*; what state means and how it merges is owned by
the tracker flavor.
