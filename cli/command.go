// Package cli exposes the provider's Cobra commands for standalone use or
// embedding in another application's command tree.
//
// When using the current executable as the VZ runner, call macvz.RunnerMain at
// the start of main, before executing Cobra, and sign the executable with the
// virtualization entitlement. Alternatively, select a signed runner with --runner.
package cli

import (
	"github.com/colony-2/pulse-macvz/internal/servercli"
	"github.com/spf13/cobra"
)

// NewCommand constructs the complete standalone CLI with setup, upgrade, status,
// images, launches, logs, and server subcommands. version is the host executable's version.
func NewCommand(version string) *cobra.Command {
	return servercli.NewCommand(version)
}

// NewSetupCommand constructs a setup subcommand with the standalone CLI's
// flags, configuration persistence, validation, and preparation progress.
// It inherits context, output streams, and persistent hooks from its Cobra
// parent. Configuration defaults to ~/.pulse/vz/provider.json; use --config and
// --owner to select the same configuration and pool as the running provider.
// Construct a fresh command for each command tree.
func NewSetupCommand() *cobra.Command {
	return servercli.NewSetupCommand()
}

// NewUpgradeCommand constructs an upgrade subcommand that drains accepted
// work and rebuilds prepared images. It uses the same configuration, flags,
// progress, and parent inheritance as NewSetupCommand.
func NewUpgradeCommand() *cobra.Command {
	return servercli.NewUpgradeCommand()
}

// NewStatusCommand reports the selected pool without opening a controller or
// modifying VMs/configuration. It can run alongside the active provider.
func NewStatusCommand() *cobra.Command {
	return servercli.NewStatusCommand()
}

// NewImagesCommand reports worker VMs, base images, and cached templates without
// opening a controller or modifying VMs/configuration.
func NewImagesCommand() *cobra.Command { return servercli.NewImagesCommand() }

// NewLogsCommand reads retained per-launch output without opening a controller.
func NewLogsCommand() *cobra.Command { return servercli.NewLogsCommand() }

// NewLaunchesCommand lists retained launch history and supplies show and health subcommands.
// It is read-only and can run alongside the active reverse provider.
func NewLaunchesCommand() *cobra.Command { return servercli.NewLaunchesCommand() }
