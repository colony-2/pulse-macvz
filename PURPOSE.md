# Pulse macOS Virtualization.framework provider

This repository owns `github.com/colony-2/pulse-macvz`: an importable Go library
and `pulse-macvz` Cobra CLI implementing the shared Pulse provider API. It runs
macOS and Linux ARM64 jobs on Apple Silicon using cgo-free native VZ bindings.
Tart and Orchard providers are removed; compatible OCI images remain supported.

Own image preparation, suspended macOS templates, Linux workers, Docker
companions, capability admission, durable guest delivery, recovery, and cleanup.
Pulse owns scheduling and leases; c2j owns execution and secret retrieval. Pass
opaque leases through stdin and never log their contents. Accepting a launch
means native dispatch succeeded, not that guest execution completed.

Preserve VZ storage and owner identities across restarts. Never replay ambiguous
native starts, delete unrelated VMs, or terminate accepted jobs for maintenance.
Keep the library and Cobra setup/upgrade/server behavior shared. Defaults and
configuration migration are documented in README.md and docs/server-cli.md.
