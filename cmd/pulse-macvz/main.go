package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	macvz "github.com/colony-2/pulse-macvz"
	"github.com/colony-2/pulse-macvz/cli"
)

var version = "dev"

func main() {
	macvz.RunnerMain()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.NewCommand(version).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
