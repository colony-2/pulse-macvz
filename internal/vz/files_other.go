//go:build !darwin

package vz

func cloneFile(src, dst string) error       { return ErrPlatform }
func renameExclusive(src, dst string) error { return ErrPlatform }
func hostBuild() (string, error)            { return "", ErrPlatform }
