# Suspended macOS slots

Each slot has an unused, suspended template with its own machine identifier and
MAC. Jobs resume fresh clones of that template; the template never receives a
lease. Clones retain the saved hardware identity and verify boot UUID/MAC before
lease delivery. An unresolved or active clone reserves its slot.

VZ stores ownership and recovery properties beside each VM. Close leaves VMs
intact. Reopening with the same home and owner recovers the pool. A job clone
whose native runner exited is terminal; an intentionally suspended reusable
template remains available. Snapshot recovery across incompatible host updates
is not guaranteed: drain and rebuild templates with `pulse-macvz upgrade`.

Linux uses cold clones rather than suspended templates. For measured VZ
scenario timings and qualification boundaries, see [VZ testing](vz-testing.md).
