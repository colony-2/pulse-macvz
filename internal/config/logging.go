package config

import (
	"path/filepath"
	"time"
)

// Logging controls the private host archive. Guest capture is independently
// bounded to four 16 MiB segments, with compressed rotated segments.
type Logging struct {
	Directory  string
	SegmentMiB int
	Backups    int
	MaxBytes   int64
	MaxAge     time.Duration
	Compress   *bool
}

func (c Logging) Normalize() (Logging, error) {
	if c.Directory != "" && !filepath.IsAbs(c.Directory) {
		return c, Err
	}
	if c.SegmentMiB == 0 {
		c.SegmentMiB = 16
	}
	if c.Backups == 0 {
		c.Backups = 3
	}
	if c.SegmentMiB < 1 || c.SegmentMiB > 64 || c.Backups < 1 || c.Backups > 16 {
		return c, Err
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 1 << 30
	}
	if c.MaxAge == 0 {
		c.MaxAge = 7 * 24 * time.Hour
	}
	if c.MaxBytes < 1<<20 || c.MaxAge < time.Minute {
		return c, Err
	}
	compress := true
	if c.Compress != nil {
		compress = *c.Compress
	}
	c.Compress = &compress
	return c, nil
}
