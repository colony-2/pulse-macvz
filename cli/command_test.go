package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/cli"
	"github.com/spf13/cobra"
)

func TestEmbeddedManagementCommands(t *testing.T) {
	for _, constructor := range []func() *cobra.Command{cli.NewSetupCommand, cli.NewUpgradeCommand} {
		name := constructor().Name()
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			var output bytes.Buffer
			root := &cobra.Command{Use: "pulse", SilenceErrors: true, SilenceUsage: true}
			group := &cobra.Command{Use: "vz"}
			group.AddCommand(constructor())
			root.AddCommand(group)
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{"vz", name, "--help"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"pulse vz " + name, "--macos-base", "--linux-capacity", "--progress", "--config", "--timeout"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("embedded help missing %q", want)
				}
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("help touched storage: %v, %v", entries, err)
			}

			// Both exported commands retain validation before opening a backend.
			root.SetArgs([]string{"vz", name, "--backend", "invalid"})
			group.RemoveCommand(group.Commands()[0])
			group.AddCommand(constructor())
			if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--backend must be") {
				t.Fatalf("expected backend validation, got %v", err)
			}
		})
	}
}
