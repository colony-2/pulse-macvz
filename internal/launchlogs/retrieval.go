package launchlogs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	api "github.com/colony-2/pulse-provider-api"
)

var launchIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type logCheckpoint struct {
	Version int    `json:"v"`
	Scope   string `json:"scope"`
	Launch  string `json:"launch"`
	Offset  uint64 `json:"offset"`
}

// GetLaunchLog reads only the durable host archive. It never contacts a guest,
// reconciles capacity, starts capture, or depends on a live Submit listener.
func (a *Archive) GetLaunchLog(ctx context.Context, q api.LaunchLogRequest) (api.LaunchLogResponse, error) {
	return ReadLaunchLog(ctx, a.root, q)
}

// ReadLaunchLog pages a retained archive without modifying it. Root identifies
// the provider's owner scope and remains part of checkpoints across restarts.
func ReadLaunchLog(ctx context.Context, root string, q api.LaunchLogRequest) (api.LaunchLogResponse, error) {
	if err := ctx.Err(); err != nil {
		return api.LaunchLogResponse{}, err
	}
	if !launchIDPattern.MatchString(q.LaunchID) || q.PageSize < 0 || q.PageSize > 100 || len(q.PageToken) > 16384 {
		return api.LaunchLogResponse{}, api.ErrLaunchLogInvalidRequest
	}
	if q.PageSize == 0 {
		q.PageSize = 100
	}
	// Existence precedes cursor interpretation, after request envelope validation.
	first, err := logstore.ReadRecordsContext(ctx, root, q.LaunchID, 0)
	if err != nil {
		return api.LaunchLogResponse{}, err
	}
	if first.Pending || first.Manifest.End > 0 && len(first.Frames) == 0 {
		return api.LaunchLogResponse{}, api.ErrLaunchLogNotFound
	}
	if err := ctx.Err(); err != nil {
		return api.LaunchLogResponse{}, err
	}
	scope := sha256.Sum256([]byte(root))
	cp := logCheckpoint{Version: 1, Scope: hex.EncodeToString(scope[:]), Launch: q.LaunchID}
	if len(q.PageToken) > 0 {
		raw, e := base64.RawURLEncoding.DecodeString(q.PageToken)
		var saved logCheckpoint
		if e != nil || json.Unmarshal(raw, &saved) != nil || saved.Version != cp.Version || saved.Scope != cp.Scope || saved.Launch != cp.Launch || saved.Offset > first.Manifest.End {
			return api.LaunchLogResponse{}, api.ErrLaunchLogCursor
		}
		cp = saved
	}
	out := api.LaunchLogResponse{Records: make([]api.LaunchLogRecord, 0), Finalized: first.Manifest.Closed || first.Manifest.Interrupted || first.Manifest.Unavailable, Incomplete: first.Gap || first.Manifest.Lost || first.Manifest.Interrupted || first.Manifest.Unavailable}
	// Missing legacy metadata applies to the whole log, including empty tail
	// polls. Capture format is fixed for a launch's lifetime.
	for _, f := range first.Frames {
		out.Incomplete = out.Incomplete || f.Sequence == 0 || f.Timestamp.IsZero()
	}
	p := first
	if cp.Offset != 0 {
		p, err = logstore.ReadRecordsContext(ctx, root, q.LaunchID, cp.Offset)
		if errors.Is(err, logstore.ErrOffset) {
			return api.LaunchLogResponse{}, api.ErrLaunchLogCursor
		}
		if err != nil {
			return api.LaunchLogResponse{}, err
		}
	}
	if q.PageToken != "" && (len(p.Frames) > 0 && p.Frames[0].Offset != cp.Offset || len(p.Frames) == 0 && cp.Offset < p.Manifest.End) {
		return api.LaunchLogResponse{}, api.ErrLaunchLogCursor
	}
	for {
		if err := ctx.Err(); err != nil {
			return api.LaunchLogResponse{}, err
		}
		if p.Pending {
			return api.LaunchLogResponse{}, api.ErrLaunchLogNotFound
		}
		out.Incomplete = out.Incomplete || p.Gap || p.Manifest.Lost || p.Manifest.Interrupted || p.Manifest.Unavailable
		out.Finalized = p.Manifest.Closed || p.Manifest.Interrupted || p.Manifest.Unavailable
		for _, f := range p.Frames {
			if len(out.Records) == q.PageSize {
				out.HasMore = true
				break
			}
			seq := f.Sequence
			if seq == 0 {
				// Legacy frames have byte offsets but no stored record number or
				// capture time. Preserve their bytes and unknown (zero) timestamp;
				// offset+1 is stable across retention/restarts, with gaps disclosed.
				seq = f.Offset + 1
				out.Incomplete = true
			}
			if f.Timestamp.IsZero() {
				out.Incomplete = true
			}
			if seq == 0 || len(out.Records) > 0 && seq <= out.Records[len(out.Records)-1].Sequence {
				return api.LaunchLogResponse{}, logstore.ErrJournal
			}
			out.Records = append(out.Records, api.LaunchLogRecord{Sequence: seq, Timestamp: f.Timestamp, Source: api.LogSourceCombined, Data: append([]byte(nil), f.Data...)})
			cp.Offset = f.Offset + uint64(len(f.Data))
		}
		if out.HasMore || p.Next == p.Manifest.End {
			if !out.HasMore {
				cp.Offset = p.Next
			}
			break
		}
		p, err = logstore.ReadRecordsContext(ctx, root, q.LaunchID, p.Next)
		if err != nil {
			return api.LaunchLogResponse{}, err
		}
	}
	if !out.Incomplete && first.Manifest.End <= math.MaxInt64 {
		n := int64(first.Manifest.End)
		out.EstimatedTotalSizeBytes = &n
	}
	raw, _ := json.Marshal(cp)
	out.NextPageToken = base64.RawURLEncoding.EncodeToString(raw)
	return out, nil
}
