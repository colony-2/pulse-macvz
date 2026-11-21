package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/backend"
	api "github.com/colony-2/pulse-provider-api"
)

type LogTarget struct {
	LaunchID string     `json:"launch_id"`
	Name     string     `json:"vm_name"`
	UID      string     `json:"vm_uid"`
	Job      api.JobRef `json:"job"`
	Phase    string     `json:"phase"`
}

func (p *Provider) LogTargets(ctx context.Context) ([]LogTarget, error) {
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	defer p.Store.mu.Unlock()
	out := make([]LogTarget, 0, len(p.Store.data.Records))
	for _, r := range p.Store.data.Records {
		if r.UID == "" {
			continue
		}
		job := r.Job
		job.JobDB = ""
		out = append(out, LogTarget{r.LaunchID, r.Name, r.UID, job, r.Phase})
	}
	return out, nil
}

var ErrLogsUnavailable = errors.New("guest log helper unavailable; upgrade prepared images")
var ErrLogsGone = errors.New("execution VM no longer available")

// Logs reads only from the exact native identity recorded for this launch.
func (p *Provider) Logs(ctx context.Context, t LogTarget, offset uint64) (logstore.Page, error) {
	b, ok := p.Backend.(interface {
		Exec(context.Context, string, string, []byte) ([]byte, error)
	})
	if !ok {
		return logstore.Page{}, ErrLogsUnavailable
	}
	v, e := p.Backend.Get(ctx, t.Name)
	if backend.IsStatus(e, 404) {
		return logstore.Page{}, ErrLogsGone
	}
	if e != nil {
		return logstore.Page{}, e
	}
	if v.UID != t.UID || v.Stopped() {
		return logstore.Page{}, ErrLogsGone
	}
	q, _ := json.Marshal(logstore.Request{LaunchID: t.LaunchID, Offset: offset})
	script := `set -eu
helper=/opt/pulse-tart/bin/pulse-tart-guest
version=$(sudo -n "$helper" version)
if [ "$version" != 'pulse-macvz-guest 0.2.0' ]; then echo PULSE_LOG_UNAVAILABLE; exit 0; fi
sudo -n "$helper" logs <<'PULSE_LOG_REQUEST'
` + string(q) + "\nPULSE_LOG_REQUEST\n"
	out, e := b.Exec(ctx, t.Name, t.UID, []byte(script))
	if e != nil {
		return logstore.Page{}, e
	}
	if strings.TrimSpace(string(out)) == "PULSE_LOG_UNAVAILABLE" {
		return logstore.Page{}, ErrLogsUnavailable
	}
	var page logstore.Page
	if len(out) > 128<<10 || json.Unmarshal(out, &page) != nil {
		return page, logstore.ErrJournal
	}
	if !page.Pending && (page.Manifest.Version != 1 || page.Manifest.LaunchID != t.LaunchID || page.Next < offset || page.Next > page.Manifest.End) {
		return page, logstore.ErrJournal
	}
	bytes := 0
	next := offset
	for _, f := range page.Frames {
		bytes += len(f.Data)
		if len(f.Data) == 0 || len(f.Data) > logstore.Chunk || f.Offset < next || f.CRC != crc32.ChecksumIEEE(f.Data) || f.Offset+uint64(len(f.Data)) < f.Offset {
			return page, logstore.ErrJournal
		}
		if f.Offset != next {
			page.Gap = true
		}
		next = f.Offset + uint64(len(f.Data))
	}
	if bytes > logstore.Batch || len(page.Frames) > 16 || next > page.Next {
		return page, logstore.ErrJournal
	}
	if !page.Pending && page.Next != next {
		page.Gap = true
	}
	return page, nil
}
func (p *Provider) AckLogs(ctx context.Context, t LogTarget, end uint64) error {
	b, ok := p.Backend.(interface {
		Exec(context.Context, string, string, []byte) ([]byte, error)
	})
	if !ok {
		return nil
	}
	q, _ := json.Marshal(logstore.Request{LaunchID: t.LaunchID, Offset: end})
	_, e := b.Exec(ctx, t.Name, t.UID, []byte(fmt.Sprintf("sudo -n /opt/pulse-tart/bin/pulse-tart-guest logs-ack <<'PULSE_LOG_REQUEST'\n%s\nPULSE_LOG_REQUEST\n", q)))
	return e
}
