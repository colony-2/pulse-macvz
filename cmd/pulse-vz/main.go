// Command pulse-vz is an optional standalone runner for hosts that do not embed
// RunnerMain. Build with CGO_ENABLED=0 and sign with the virtualization entitlement.
package main

import (
	"fmt"
	"github.com/colony-2/pulse-tart/internal/vz"
	"os"
)

func main() {
	vz.RunnerMain()
	fmt.Fprintln(os.Stderr, "pulse-vz is a provider helper; configure VZConfig.Runner with its absolute path")
	os.Exit(2)
}
