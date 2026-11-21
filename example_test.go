package macvz_test

import (
	"fmt"

	macvz "github.com/colony-2/pulse-macvz"
	api "github.com/colony-2/pulse-provider-api"
)

func ExampleConfig() {
	// The same value is passed to NewVZ. The host obtains
	// credentials and paths through its own configuration mechanism.
	cfg := macvz.Config{
		OwnerID: "pulse-macos", StateDir: "/var/lib/pulse/macos",
	}
	resolved, err := macvz.ResolveConfig(cfg)
	if err != nil {
		panic(err)
	}
	fmt.Println(*resolved.VMCounts.MacOS, *resolved.VMCounts.Linux, *resolved.VMCounts.Total, macvz.DefaultCPU, macvz.DefaultMemoryMiB)
	var _ api.Provider = (*macvz.Runtime)(nil)
	// Output: 2 4 6 4 8192
}
