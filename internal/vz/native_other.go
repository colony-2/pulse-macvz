//go:build !darwin || !arm64

package vz

func checkNative() error                                { return ErrPlatform }
func nestedVirtualizationSupported() bool               { return false }
func newIdentity() ([]byte, error)                      { return nil, ErrPlatform }
func runNative(string, diskConfig, <-chan string) error { return ErrPlatform }
