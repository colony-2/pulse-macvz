package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tart "github.com/colony-2/pulse-tart"
	"github.com/colony-2/pulse-tart/internal/servercli"
)

var version = "dev"

func main() {
	tart.RunnerMain()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := servercli.NewCommand(version).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
