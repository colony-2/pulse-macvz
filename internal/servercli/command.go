package servercli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	tart "github.com/colony-2/pulse-tart"
	"github.com/spf13/cobra"
)

type serverOptions struct {
	mode, backend, owner, stateDir, vmHome, runner, tartBinary string
	c2j                                                        string
	capacity                                                   capacityOptions
	poll, bootstrap                                            time.Duration
	endpoint                                                   tart.DockerEndpoint
	orchard                                                    tart.OrchardConfig
	orchardTokenFile                                           string
	config                                                     tart.Config
	transport                                                  transportOptions
	version                                                    string
	log                                                        io.Writer
}

// NewCommand constructs the standalone CLI. Help and validation create no VMs.
func NewCommand(version string) *cobra.Command {
	cmd := newCommand(runServer)
	cmd.Version = version
	return cmd
}

func newCommand(run func(context.Context, serverOptions) error) *cobra.Command {
	root := &cobra.Command{Use: "pulse-tart", Short: "Run a Pulse VM provider", SilenceUsage: true, SilenceErrors: true}
	var o serverOptions
	server := &cobra.Command{
		Use:   "server",
		Short: "Serve a VM provider through Pulse's outbound or inbound plugin",
		Long:  "Run a VM provider and reconcile its images and jobs. Outbound mode connects to a Pulse collector using mutual TLS. Inbound mode is launched by the host through the plugin SDK (currently Linux only). Stopping the server leaves accepted VM jobs running for recovery on restart.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.capacity.totalSet = cmd.Flags().Changed("total-capacity")
			o.version = cmd.Root().Version
			if o.version == "" {
				o.version = "dev"
			}
			o.log = cmd.ErrOrStderr()
			if err := o.resolve(); err != nil {
				return err
			}
			return run(cmd.Context(), o)
		},
	}
	f := server.Flags()
	o.transport.flags(f)
	f.StringVarP(&o.mode, "mode", "m", "outbound", "Plugin mode: outbound or inbound")
	f.StringVarP(&o.backend, "backend", "b", "vz", "VM provider: vz, tart, or orchard")
	f.StringVarP(&o.owner, "owner", "o", "server", "Stable ownership scope; reuse it when restarting")
	f.StringVarP(&o.stateDir, "state-dir", "s", "", "Controller state directory (default ~/.pulse-tart/BACKEND/OWNER)")
	f.StringVar(&o.vmHome, "vm-home", "", "VZ or Tart VM home (default: backend's own home)")
	f.StringVar(&o.runner, "runner", "", "VZ runner executable (default: this signed server executable)")
	f.StringVar(&o.tartBinary, "tart-binary", "tart", "Tart executable")
	f.StringVar(&o.capacity.macBase, "macos-base", tart.DefaultBases()[0].Name, "macOS base: macos-26-arm64 or macos-15-arm64")
	f.IntVar(&o.capacity.macOS, "macos-capacity", tart.DefaultMacOSVMCount, "Concurrent macOS jobs (0–2)")
	f.IntVarP(&o.capacity.linux, "linux-capacity", "l", tart.DefaultLinuxVMCount, "Concurrent Linux job VMs, each including Docker (0–1000)")
	f.IntVarP(&o.capacity.macDocker, "macos-docker-capacity", "d", 0, "Reserve this part of macOS capacity for jobs with a Linux Docker companion")
	f.IntVarP(&o.capacity.total, "total-capacity", "c", 0, "Shared job limit (default: macOS + Linux capacity; explicit 0 drains all jobs)")
	f.StringVar(&o.c2j, "c2j-version", "latest", "c2j release for preparing missing images")
	f.DurationVar(&o.poll, "poll-interval", 2*time.Second, "Provider reconciliation interval")
	f.DurationVar(&o.bootstrap, "bootstrap-timeout", 45*time.Minute, "Guest image preparation timeout")
	f.StringVar(&o.endpoint.Address, "docker-worker-address", "192.168.64.1", "Host/worker IPv4 address reachable by macOS Docker guests")
	f.IntVar(&o.endpoint.PortMin, "docker-port-min", 45000, "First reserved Docker companion bridge port")
	f.IntVar(&o.endpoint.PortMax, "docker-port-max", 45099, "Last reserved Docker companion bridge port")
	f.StringVar(&o.orchard.URL, "orchard-url", "", "Orchard API URL (required for orchard backend)")
	f.StringVar(&o.orchard.Worker, "orchard-worker", "", "Orchard worker name")
	f.StringVar(&o.orchard.Username, "orchard-username", "", "Orchard API username")
	f.StringVar(&o.orchardTokenFile, "orchard-token-file", "", "File containing the Orchard API token")
	f.BoolVar(&o.orchard.AllowInsecureHTTP, "orchard-allow-http", false, "Allow unencrypted HTTP to an Orchard lab server")
	root.AddCommand(server)
	return root
}

func (o *serverOptions) resolve() error {
	if o.mode != "outbound" && o.mode != "inbound" {
		return fmt.Errorf("--mode must be outbound or inbound")
	}
	if o.backend != "vz" && o.backend != "tart" && o.backend != "orchard" {
		return fmt.Errorf("--backend must be vz, tart, or orchard")
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
		o.stateDir = filepath.Join(home, ".pulse-tart", o.backend, o.owner)
	}
	var err error
	for _, value := range []*string{&o.stateDir, &o.vmHome, &o.runner, &o.orchardTokenFile} {
		if *value, err = absolutePath(*value); err != nil {
			return err
		}
	}
	o.config, err = o.capacity.config(o.owner, o.stateDir, o.c2j, o.poll, o.bootstrap)
	if err != nil {
		return err
	}
	if o.config.NeedsDockerEndpoint() {
		if err := o.endpoint.Validate(); err != nil {
			return fmt.Errorf("invalid Docker worker IPv4 address or port range: %w", err)
		}
		if o.endpoint.PortMax-o.endpoint.PortMin+1 < o.capacity.macDocker {
			return fmt.Errorf("Docker port range is smaller than --macos-docker-capacity")
		}
	}
	if o.backend == "orchard" {
		if o.orchard.URL == "" || o.orchard.Worker == "" {
			return fmt.Errorf("orchard requires --orchard-url and --orchard-worker")
		}
		if o.vmHome != "" || o.runner != "" {
			return fmt.Errorf("--vm-home and --runner apply only to local backends")
		}
	}
	if o.backend == "tart" && o.runner != "" {
		return fmt.Errorf("--runner requires --backend=vz")
	}
	o.orchard.DockerEndpoint = o.endpoint
	return o.transport.validate(o.mode, o.owner)
}
