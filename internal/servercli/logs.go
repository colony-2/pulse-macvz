package servercli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/spf13/cobra"
)

// NewLogsCommand reads retained output without acquiring the pool controller lock.
func NewLogsCommand() *cobra.Command {
	var id string
	var offset uint64
	var cmd *cobra.Command
	cmd = providerCommand("logs", "Read retained combined worker output for a launch", func(ctx context.Context, o serverOptions) error {
		root := o.logDirectory
		if root == "" {
			root = filepath.Join(o.vmHome, "logs")
		}
		settings := config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome}
		root = filepath.Join(root, settings.Scope())
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			p, err := logstore.Read(root, id, offset)
			if err != nil {
				return err
			}
			if p.Pending {
				return fmt.Errorf("no retained output for launch %s", id)
			}
			if p.Gap {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning: part of this launch's output is no longer retained")
			}
			for _, f := range p.Frames {
				if _, err := cmd.OutOrStdout().Write(f.Data); err != nil {
					return err
				}
			}
			offset = p.Next
			if offset >= p.Manifest.End {
				if p.Manifest.Lost {
					fmt.Fprintln(cmd.ErrOrStderr(), "Warning: output capture reported loss")
				}
				if !p.Manifest.Closed {
					fmt.Fprintln(cmd.ErrOrStderr(), "Note: output is not known to be complete")
				}
				return nil
			}
		}
	})
	cmd.Flags().StringVarP(&id, "launch-id", "i", "", "Launch ID (required)")
	cmd.Flags().Uint64Var(&offset, "offset", 0, "Start at this raw byte offset")
	_ = cmd.MarkFlagRequired("launch-id")
	return cmd
}
