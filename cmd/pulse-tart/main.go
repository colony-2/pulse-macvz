package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tart "github.com/colony-2/pulse-tart"
	"github.com/colony-2/pulse-tart/cli"
)

var version = "dev"

func main() {
	tart.RunnerMain()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.NewCommand(version).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
