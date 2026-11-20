package backend

import (
	"context"
	"time"
)

// TransferProgress counts compressed registry bytes, not sparse disk allocation.
// Total is zero until the manifest is known. Completion includes verification.
type TransferProgress struct {
	Stage          string
	Current, Total int64
}

type SourceProgress struct {
	Source string
	TransferProgress
	Started, Updated time.Time
}

// VMProgress deliberately excludes credentials, scripts and guest output.
type VMProgress struct {
	Name, Stage, Cause string
}

type PreparationProgress struct {
	Sources []SourceProgress
	VMs     []VMProgress
}

type progressKey struct{}

func WithProgress(ctx context.Context, report func(TransferProgress)) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

func ReportProgress(ctx context.Context, progress TransferProgress) {
	if report, ok := ctx.Value(progressKey{}).(func(TransferProgress)); ok {
		report(progress)
	}
}
