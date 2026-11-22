package servercli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	macvz "github.com/colony-2/pulse-macvz"
	"github.com/spf13/cobra"
)

type serverOptions struct {
	mode, backend, owner, stateDir, vmHome, runner string
	c2j                                            string
	logDirectory                                   string
	logging                                        macvz.LoggingConfig
	logCompress                                    bool
	capacity                                       capacityOptions
	poll, bootstrap                                time.Duration
	endpoint                                       macvz.DockerEndpoint
	config                                         macvz.Config
	transport                                      transportOptions
	configPath                                     string
	managementTimeout                              time.Duration
	progressMode                                   string
	version                                        string
	log                                            io.Writer
}

// NewCommand constructs the standalone CLI. Help and validation create no VMs.
func NewCommand(version string) *cobra.Command {
	cmd := newCommand(runServer)
	cmd.Version = version
	return cmd
}

func newCommand(run func(context.Context, serverOptions) error) *cobra.Command {
	root := &cobra.Command{Use: "pulse-macvz", Short: "Run a Pulse VM provider", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(providerCommand("server", "Serve a VM provider through Pulse's outbound or inbound plugin", run))
	root.AddCommand(NewSetupCommand(), NewUpgradeCommand(), NewStatusCommand(), NewImagesCommand(), NewLogsCommand(), NewLaunchesCommand())
	return root
}

// NewSetupCommand constructs the shared setup subcommand.
func NewSetupCommand() *cobra.Command {
	return providerCommand("setup", "Prepare or resume configured VM capacity, reusing completed images", runSetup)
}

// NewUpgradeCommand constructs the shared upgrade subcommand.
func NewUpgradeCommand() *cobra.Command {
	return providerCommand("upgrade", "Drain accepted work and rebuild this owner's prepared images", runUpgrade)
}

func providerCommand(name, short string, run func(context.Context, serverOptions) error) *cobra.Command {
	var o serverOptions
	server := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := loadConfiguration(cmd, &o); err != nil {
				return err
			}
			o.capacity.totalSet = cmd.Flags().Changed("total-capacity")
			o.version = cmd.Root().Version
			if o.version == "" {
				o.version = "dev"
			}
			o.log = cmd.ErrOrStderr()
			resolve := o.resolve
			if inspectionCommand(name) {
				resolve = o.resolvePool
			}
			if err := resolve(); err != nil {
				return err
			}
			if err := run(cmd.Context(), o); err != nil {
				return err
			}
			if name != "server" && !inspectionCommand(name) {
				return saveConfiguration(cmd, o.configPath)
			}
			return nil
		},
	}
	f := server.Flags()
	f.StringVar(&o.logDirectory, "log-directory", "", "Private per-launch log archive (default <vm-home>/logs)")
	f.IntVar(&o.logging.SegmentMiB, "log-segment-mib", 16, "Guest log rotation size in MiB (1–64)")
	f.IntVar(&o.logging.Backups, "log-backups", 3, "Retained rotated guest log segments (1–16)")
	f.BoolVar(&o.logCompress, "log-compress", true, "Compress rotated guest logs and host archive batches")
	f.Int64Var(&o.logging.MaxBytes, "log-max-bytes", 1<<30, "Host retained-log byte budget per owner scope")
	f.DurationVar(&o.logging.MaxAge, "log-max-age", 7*24*time.Hour, "Completed host log retention")
	f.StringVarP(&o.configPath, "config", "f", "", "Configuration file (default ~/.pulse/vz/provider.json; explicit flags override saved settings)")
	if name != "server" && !inspectionCommand(name) {
		f.DurationVar(&o.managementTimeout, "timeout", 2*time.Hour, "Maximum preparation/drain time; timeout never cancels accepted jobs")
		f.StringVar(&o.progressMode, "progress", "auto", "Preparation display: auto, interactive, or linear (redirected output is always linear)")
	}
	o.transport.flags(f)
	f.StringVarP(&o.backend, "backend", "b", "vz", "VM provider (vz)")
	f.StringVarP(&o.mode, "mode", "m", "outbound", "Plugin mode: outbound or inbound")
	f.StringVarP(&o.owner, "owner", "o", "server", "Stable ownership scope; reuse it when restarting")
	f.StringVarP(&o.stateDir, "state-dir", "s", "", "Controller state directory (default ~/.pulse/vz/state/OWNER)")
	f.StringVar(&o.vmHome, "vm-home", "", "VM home (default ~/.pulse/vz)")
	f.StringVar(&o.runner, "runner", "", "VZ runner executable (default: this signed server executable)")
	f.StringVar(&o.capacity.macBase, "macos-base", "all", "macOS bases: all, or comma-separated macos-26-arm64,macos-15-arm64")
	f.IntVar(&o.capacity.macOS, "macos-capacity", macvz.DefaultMacOSVMCount, "Concurrent macOS jobs (0–2)")
	f.IntVarP(&o.capacity.linux, "linux-capacity", "l", macvz.DefaultLinuxVMCount, "Concurrent Linux job VMs; Docker starts only when requested (0–1000)")
	f.IntVarP(&o.capacity.macDocker, "macos-docker-capacity", "d", -1, "macOS slots eligible for Docker per version (-1: all; companions start only when requested)")
	f.IntVarP(&o.capacity.total, "total-capacity", "c", 0, "Shared job limit (default: macOS + Linux capacity; explicit 0 drains all jobs)")
	f.StringVar(&o.c2j, "c2j-version", "latest", "c2j release for preparing missing images")
	f.DurationVar(&o.poll, "poll-interval", 2*time.Second, "Provider reconciliation interval")
	f.DurationVar(&o.bootstrap, "bootstrap-timeout", 45*time.Minute, "Guest image preparation timeout")
	f.StringVar(&o.endpoint.Address, "docker-worker-address", "192.168.64.1", "Host/worker IPv4 address reachable by macOS Docker guests")
	f.IntVar(&o.endpoint.PortMin, "docker-port-min", 45000, "First reserved Docker companion bridge port")
	f.IntVar(&o.endpoint.PortMax, "docker-port-max", 45099, "Last reserved Docker companion bridge port")
	return server
}

func (o *serverOptions) resolve() error {
	if err := o.resolvePool(); err != nil {
		return err
	}
	if o.managementTimeout != 0 && o.mode == "outbound" && !o.transport.configured() {
		return o.transport.validate("inbound", o.owner)
	}
	return o.transport.validate(o.mode, o.owner)
}

func (o *serverOptions) resolvePool() error {
	if o.progressMode != "" && o.progressMode != "auto" && o.progressMode != "interactive" && o.progressMode != "linear" {
		return fmt.Errorf("--progress must be auto, interactive, or linear")
	}
	if o.mode != "outbound" && o.mode != "inbound" {
		return fmt.Errorf("--mode must be outbound or inbound")
	}
	if o.backend != "vz" {
		return fmt.Errorf("--backend must be vz")
	}
	// Validate ownership before using it as a path component.
	if _, err := o.capacity.config(o.owner, "", o.c2j, o.poll, o.bootstrap); err != nil {
		return err
	}
	if o.stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		o.stateDir = filepath.Join(home, ".pulse", "vz", "state", o.owner)
	}
	var err error
	if o.vmHome == "" && o.backend == "vz" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		o.vmHome = filepath.Join(home, ".pulse", "vz")
	}
	for _, value := range []*string{&o.logDirectory, &o.stateDir, &o.vmHome, &o.runner, &o.transport.ca, &o.transport.cert, &o.transport.key, &o.transport.sharedKeyFile} {
		if *value, err = absolutePath(*value); err != nil {
			return err
		}
	}
	o.config, err = o.capacity.config(o.owner, o.stateDir, o.c2j, o.poll, o.bootstrap)
	if err != nil {
		return err
	}
	o.logging.Directory = o.logDirectory
	o.logging.Compress = &o.logCompress
	o.config.Logging, err = o.logging.Normalize()
	if err != nil {
		return fmt.Errorf("invalid log storage or retention configuration")
	}
	if o.config.NeedsDockerEndpoint() {
		if err := o.endpoint.Validate(); err != nil {
			return fmt.Errorf("invalid Docker worker IPv4 address or port range: %w", err)
		}
		if o.endpoint.PortMax-o.endpoint.PortMin+1 < min(o.capacity.macOS, *o.config.VMCounts.Total) {
			return fmt.Errorf("Docker port range is smaller than --macos-docker-capacity")
		}
	}
	return nil
}

func inspectionCommand(name string) bool {
	return name == "status" || name == "images" || name == "logs" || name == "launches" || name == "show"
}
