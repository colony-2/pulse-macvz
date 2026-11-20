package servercli

import (
	"slices"
	"testing"

	tart "github.com/colony-2/pulse-tart"
)

func TestCapacityConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		options                     capacityOptions
		plain, docker, linux, total int
	}{
		{"defaults", capacityOptions{macOS: 2, linux: 4}, 2, 0, 4, 6},
		{"one paired macOS job", capacityOptions{macOS: 2, linux: 4, macDocker: 1}, 1, 1, 4, 6},
		{"Linux only", capacityOptions{linux: 3}, 0, 0, 3, 3},
		{"disabled", capacityOptions{}, 0, 0, 0, 0},
		{"drain total", capacityOptions{macOS: 2, linux: 4, macDocker: 1, totalSet: true}, 0, 0, 0, 0},
		{"shared cap", capacityOptions{macOS: 2, linux: 4, total: 1, totalSet: true}, 1, 0, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.options.config("server", "", "", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if *c.VMCounts.Total != tc.total {
				t.Fatalf("total=%d", *c.VMCounts.Total)
			}
			for _, b := range c.Bases {
				plain, docker := 0, 0
				for _, g := range c.Slots {
					if g.Base != b.Name {
						continue
					}
					if g.CPU != tart.DefaultCPU || g.MemoryMiB != tart.DefaultMemoryMiB {
						t.Fatal("wrong resource envelope")
					}
					if g.DockerProfile == "" {
						plain += g.Count
					} else {
						docker += g.Count
					}
				}
				if b.OS == "linux" {
					if plain != 0 || docker != tc.linux {
						t.Fatalf("Linux plain=%d docker=%d", plain, docker)
					}
				} else if plain != tc.plain || docker != tc.docker {
					t.Fatalf("macOS plain=%d docker=%d", plain, docker)
				}
				if b.OS != "linux" && tc.options.macDocker == 0 && slices.Contains(c.BaseCapabilities(b.Name), "docker") {
					t.Fatal("disabled macOS Docker advertised")
				}
			}
			if c.NeedsDockerEndpoint() != (tc.docker > 0) {
				t.Fatal("incorrect host endpoint requirement")
			}
		})
	}
}

func TestInvalidCapacities(t *testing.T) {
	for _, c := range []capacityOptions{
		{macOS: -1}, {macOS: 3}, {linux: -1}, {linux: 1001},
		{macOS: 1, macDocker: 2}, {macDocker: -1},
		{total: -1, totalSet: true}, {total: 1003, totalSet: true},
	} {
		if _, err := c.config("server", "", "", 0, 0); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}
