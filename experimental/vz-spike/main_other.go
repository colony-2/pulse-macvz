//go:build !darwin || !arm64

package main

import (
	"fmt"
	"os"
)

func main() { fmt.Fprintln(os.Stderr, "vz-spike requires macOS ARM64"); os.Exit(1) }
