//go:build darwin

package vz

import "golang.org/x/sys/unix"

func cloneFile(src, dst string) error       { return unix.Clonefile(src, dst, 0) }
func renameExclusive(src, dst string) error { return unix.RenamexNp(src, dst, unix.RENAME_EXCL) }
func hostBuild() (string, error)            { return unix.Sysctl("kern.osversion") }
