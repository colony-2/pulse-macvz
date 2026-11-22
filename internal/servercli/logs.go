package servercli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/launchhistory"
	"github.com/spf13/cobra"
)

// NewLogsCommand reads the host archive, without acquiring a controller lock,
// contacting the reverse server, or running commands in a guest.
func NewLogsCommand() *cobra.Command {
	var id, argument string
	var offset uint64
	var follow bool
	var cmd *cobra.Command
	cmd = providerCommand("logs", "Read retained combined worker output for a launch", func(ctx context.Context, o serverOptions) error {
		if argument != "" {
			if id != "" && id != argument {
				return fmt.Errorf("positional launch ID conflicts with --launch-id")
			}
			id = argument
		}
		if id == "" {
			return fmt.Errorf("provide a launch ID or --launch-id")
		}
		return readLaunchLogs(ctx, logRoot(o), id, offset, follow, cmd.OutOrStdout(), cmd.ErrOrStderr())
	})
	cmd.Use = "logs [LAUNCH_ID]"
	cmd.Args = func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("provide only one launch ID")
		}
		if len(args) == 1 {
			argument = args[0]
		}
		return nil
	}
	cmd.Flags().StringVarP(&id, "launch-id", "i", "", "Launch ID (alternative to positional argument)")
	cmd.Flags().Uint64Var(&offset, "offset", 0, "Start at this raw byte offset")
	cmd.Flags().BoolVarP(&follow, "follow", "F", false, "Wait for new output until capture closes or Ctrl-C")
	return cmd
}
func readLaunchLogs(ctx context.Context, root, id string, offset uint64, follow bool, out, diagnostics io.Writer) error {
	warnedGap, warnedLoss := false, false
	seen := false
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		p, e := logstore.Read(root, id, offset)
		if e != nil {
			return fmt.Errorf("read launch logs: %w", e)
		}
		if p.Pending {
			if seen {
				return fmt.Errorf("log archive is no longer retained; last byte offset was %d", offset)
			}
			h, err := launchhistory.Read(root, id)
			if !follow || err != nil || h.CleanupObservedAt != nil || h.ObservedAt == nil {
				return fmt.Errorf("no retained output for launch %s (not captured or expired)", statusText(id))
			}
		} else {
			seen = true
			if p.Gap && !warnedGap {
				fmt.Fprintln(diagnostics, "Warning: part of this launch's output is no longer retained")
				warnedGap = true
			}
			for _, frame := range p.Frames {
				if _, e = out.Write(frame.Data); e != nil {
					return e
				}
			}
			previous := offset
			offset = p.Next
			if offset < p.Manifest.End {
				if offset == previous {
					return fmt.Errorf("log archive made no progress")
				}
				continue
			}
			if p.Manifest.Lost && !warnedLoss {
				fmt.Fprintln(diagnostics, "Warning: output capture reported loss")
				warnedLoss = true
			}
			if p.Manifest.Unavailable {
				return fmt.Errorf("guest logging helper unavailable for this launch; upgrade prepared images for future launches")
			}
			if p.Manifest.Interrupted {
				return fmt.Errorf("log capture was interrupted; retained output may be incomplete")
			}
			if p.Manifest.Closed {
				return nil
			}
			if !follow {
				fmt.Fprintln(diagnostics, "Note: log capture is still open; use --follow to wait for more output")
				return nil
			}
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
