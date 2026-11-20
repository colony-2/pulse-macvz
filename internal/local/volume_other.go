//go:build !darwin

package local

// Direct Tart is macOS-only. Retain strict device/inode checks on other hosts
// used by the mocked backend tests; do not invent a persistent volume identity.
func nativeVolumeUUID(string) (string, error) { return "", nil }
