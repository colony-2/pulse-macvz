package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	compute "github.com/colony-2/pulse-provider-api"
)

var ErrBackend = errors.New("backend observation unavailable")

type Backend interface {
	Info(context.Context) error
	List(context.Context) ([]backend.VM, error)
	Create(context.Context, backend.Spec) (backend.VM, error)
	Get(context.Context, string) (backend.VM, error)
	Delete(context.Context, string) error
}
type Provider struct {
	Config     config.Settings
	Store      *Store
	Backend    Backend
	Setup      func(context.Context) ([]byte, error)
	BaseScript []byte
	Assets     fs.FS
	// A short-lived pool preparation hint, never a queued launch or lease.
	preferredGroup    string
	preferUntil       time.Time
	diagnostic        *diagnostics
	preparationCauses map[string]preparationObservation
}

func New(c config.Settings, b Backend) (*Provider, error) {
	if _, ok := b.(templateBackend); !ok {
		return nil, backendError("backend does not support templates", nil)
	}
	s, e := OpenStore(c.StateDir, c.Scope())
	if e != nil {
		return nil, e
	}
	return Attach(c, b, s), nil
}
func Attach(c config.Settings, b Backend, s *Store) *Provider {
	return &Provider{Config: c, Store: s, Backend: b}
}
func (p *Provider) Close() error {
	p.Store.mu.Lock()
	p.preparationCauses = nil
	p.Store.mu.Unlock()
	return p.Store.Close()
}
func (p *Provider) Check(ctx context.Context) error {
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return e
	}
	broken := p.Store.broken
	cause := p.Store.stateError()
	p.Store.mu.Unlock()
	if broken {
		return cause
	}
	if e := p.Backend.Info(ctx); e != nil {
		return backendError("Check Info", e)
	}
	_, e := p.Backend.List(ctx)
	if e != nil {
		return backendError("Check inventory", e)
	}
	return nil
}
func reason(status compute.Status) string {
	switch status {
	case compute.Accepted:
		return ""
	case compute.Unknown:
		return "provisioning outcome uncertain"
	case compute.NoCapacity:
		return "no compatible allocation available"
	case compute.Unsupported:
		return "requested semantics are unsupported"
	case compute.Rejected:
		return "invalid or conflicting launch"
	default:
		return "provider unavailable before dispatch"
	}
}
func result(id string, status compute.Status) compute.Submission {
	return compute.Submission{LaunchID: id, Status: status, Reason: reason(status)}
}
func (p *Provider) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	if len(ls) < 1 || len(ls) > 100 {
		return nil, errors.New("invalid batch size")
	}
	seen := map[string]bool{}
	for _, l := range ls {
		if seen[l.LaunchID] {
			return nil, errors.New("duplicate launch ID")
		}
		seen[l.LaunchID] = true
	}
	// Serialized native dispatch bounds concurrency and makes reservation checks atomic.
	// The durable intent is saved before every native operation.
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	defer p.Store.mu.Unlock()
	if p.Store.properties != nil {
		if err := p.Store.refresh(); err != nil {
			return nil, err
		}
		if err := p.recoverPublishedSources(ctx); err != nil {
			return nil, err
		}
		inventory, err := p.Backend.List(ctx)
		if err != nil {
			return nil, backendError("admission inventory", err)
		}
		if err = p.reconcileRecords(ctx, inventory); err != nil {
			return nil, err
		}
	}
	out := make([]compute.Submission, 0, len(ls))
	defer func() { p.diagnostic = nil }()
	for _, l := range ls {
		p.diagnostic = &diagnostics{}
		if p.Store.broken {
			p.observeError("journal unavailable", p.Store.stateError())
			if old := p.Store.data.Records[l.LaunchID]; old != nil && old.diagnostic != "" {
				p.observe("earlier dispatch observation: %s", old.diagnostic)
			}
			status := compute.Unavailable
			if p.Store.data.Records[l.LaunchID] != nil {
				status = compute.Unknown
			}
			out = append(out, p.submission(l.LaunchID, status))
			continue
		}
		if condition := launchValidation(l); condition != "" {
			p.observe("launch validation failed: %s", condition)
			out = append(out, p.submission(l.LaunchID, compute.Rejected))
			continue
		}
		l = copyLaunch(l)
		hash := p.launchHash(l)
		if old := p.Store.data.Records[l.LaunchID]; old != nil {
			if p.Store.properties != nil {
				p.observe("launch ID already belongs to an existing VM; Pulse must submit each launch only once")
				out = append(out, p.submission(l.LaunchID, compute.Rejected))
				continue
			}
			status := old.Outcome
			if old.Fingerprint != hash {
				status = compute.Rejected
				p.observe("launch fingerprint conflicts with recorded assignment")
			} else if status != compute.Accepted {
				if old.diagnostic != "" {
					p.observe("%s", old.diagnostic)
				} else {
					p.observe("recorded %s outcome; historical cause unavailable", old.Phase)
				}
				if old.PairID != "" {
					p.earlierPreparation("pair:" + old.PairID)
				}
			}
			out = append(out, p.submission(l.LaunchID, status))
			continue
		}
		if l.StartBefore != nil && !time.Now().Before(*l.StartBefore) {
			p.observe("start deadline expired")
			out = append(out, p.submission(l.LaunchID, compute.Rejected))
			continue
		}
		if ctx.Err() != nil || p.Store.broken {
			p.observeError("before dispatch", ctx.Err())
			out = append(out, p.submission(l.LaunchID, compute.Unavailable))
			continue
		}
		if health, ok := p.Backend.(interface{ AdmissionHealth(context.Context) error }); ok {
			if err := health.AdmissionHealth(ctx); err != nil {
				p.observeError("delivery health prevents new admission", err)
				out = append(out, p.submission(l.LaunchID, compute.Unavailable))
				continue
			}
		}
		if submission, handled := p.submitDocker(ctx, l, hash); handled {
			out = append(out, submission)
			continue
		}
		spec, status := p.plan(l)
		if status != compute.Accepted {
			out = append(out, p.submission(l.LaunchID, status))
			continue
		}
		active := 0
		for _, r := range p.Store.data.Records {
			if r.Phase != "done" {
				active++
			}
		}
		p.observe("journal reservation occupancy=%d configured slot budget=%d", active, p.Config.Capacity())
		if active >= p.Config.Capacity() {
			out = append(out, p.submission(l.LaunchID, compute.NoCapacity))
			continue
		}
		// A fresh read failure is fallback-safe because no intent was dispatched.
		inventory, e := p.Backend.List(ctx)
		if e != nil {
			p.observeError("admission inventory unavailable; later readiness/count/physical observations unknown", e)
			out = append(out, p.submission(l.LaunchID, compute.Unavailable))
			continue
		}
		if ctx.Err() != nil {
			p.observeError("after admission inventory", ctx.Err())
			out = append(out, p.submission(l.LaunchID, compute.Unavailable))
			continue
		}
		slotID := ""
		if len(p.Config.Slots) > 0 {
			spec, slotID, status = p.chooseSlot(ctx, l, spec, inventory)
			if status != compute.Accepted {
				if status == compute.NoCapacity {
					if e := p.requestJobRoom(ctx, l); e != nil {
						p.observeError("room request", e)
						status = compute.Unavailable
					}
				}
				out = append(out, p.submission(l.LaunchID, status))
				continue
			}
		}
		available, e := p.canReserveJobs(ctx, spec)
		if e != nil {
			p.observeError("reservation check", e)
			out = append(out, p.submission(l.LaunchID, compute.Unavailable))
			continue
		}
		if !available {
			if e := p.requestJobRoom(ctx, l); e != nil {
				p.observeError("room request", e)
				out = append(out, p.submission(l.LaunchID, compute.Unavailable))
				continue
			}
			out = append(out, p.submission(l.LaunchID, compute.NoCapacity))
			continue
		}
		r := &Record{LaunchID: l.LaunchID, Fingerprint: hash, NativeHash: p.Store.hash(spec), Name: spec.Name, Job: l.Job, Created: time.Now().UTC(), Outcome: compute.Unknown, Phase: "dispatch", SlotID: slotID}
		p.Store.data.Records[l.LaunchID] = r
		p.preferredGroup = ""
		if e := p.Store.save(); e != nil {
			p.observeError("save dispatch intent", e)
			r.diagnostic = p.diagnosticText()
			out = append(out, p.submission(l.LaunchID, compute.Unknown))
			continue
		}
		v, e := p.Backend.Create(ctx, spec)
		if e == nil && v.UID != "" && v.Name == r.Name && p.Store.hash(normalized(v.Spec)) == r.NativeHash {
			r.UID = v.UID
			r.Outcome = compute.Accepted
			r.Phase = "active"
		}
		if e != nil {
			p.observeError("native create", e)
		} else if r.Outcome != compute.Accepted {
			p.observe("native create identity missing or mismatched")
		} else {
			p.observe("native create returned matching identity; result durability not yet confirmed")
		}
		// Even a non-2xx response can be generated by an intermediary after dispatch.
		// Reconcile by unique name; never blindly retry POST.
		if e := p.Store.save(); e != nil {
			p.observeError("save native create result", e)
			r.diagnostic = p.diagnosticText()
			out = append(out, p.submission(l.LaunchID, compute.Unknown))
			continue
		}
		if r.Outcome != compute.Accepted {
			r.diagnostic = p.diagnosticText()
		}
		out = append(out, p.submission(l.LaunchID, r.Outcome))
	}
	return out, nil
}
func (p *Provider) plan(l compute.Launch) (backend.Spec, compute.Status) {
	if condition := unsupportedSemantics(l); condition != "" {
		p.observe("%s", condition)
		return backend.Spec{}, compute.Unsupported
	}
	cpu := (l.CPUMillis + 999) / 1000
	memory := (l.MemoryBytes + (1 << 20) - 1) / (1 << 20)
	for _, g := range p.Config.Slots {
		if g.DockerProfile != "" && p.base(g.Base).OS == "linux" {
			continue
		}
		b := p.base(g.Base)
		if !p.baseFits(b, l) {
			p.observe("group %s configured base/capability mismatch", g.Name)
			continue
		}
		p.observe("group %s configured CPU request=%d limit=%d", g.Name, cpu, g.CPU)
		if g.CPU < cpu {
			continue
		}
		p.observe("group %s configured memory MiB request=%d limit=%d", g.Name, memory, g.MemoryMiB)
		if g.MemoryMiB >= memory {
			return backend.Spec{Name: "pulse-" + p.Config.Scope()[:12] + "-" + newID(), CPU: cpu, Memory: memory}, compute.Accepted
		}
	}
	return backend.Spec{}, compute.Unsupported
}
func startup(envelope []byte) string {
	return fmt.Sprintf(`#!/bin/bash
set -eu
set +x
umask 077
work=$(mktemp -d /tmp/pulse-bootstrap.XXXXXX)
trap 'rm -rf "$work"' EXIT
# Tools already live in the suspended image. Never reinstall them on a job.
sudo -n /bin/bash /opt/pulse-tart-image/bootstrap-guest.sh
/usr/bin/base64 -D > "$work/envelope.json" <<'PULSE_ENVELOPE'
%s
PULSE_ENVELOPE
sudo -n /opt/pulse-tart/bin/pulse-tart-guest ingest < "$work/envelope.json"
echo PULSE_HANDOFF_OK
while ! sudo -n test -f /var/db/pulse-tart/complete; do sleep 1; done
if [ "$(sudo -n cat /var/db/pulse-tart/complete)" = 0 ]; then echo PULSE_WORKER_OK; else echo PULSE_WORKER_FAILED; fi
`, base64.StdEncoding.EncodeToString(envelope))
}
func normalized(v backend.Spec) backend.Spec {
	if v.Labels == nil {
		v.Labels = map[string]string{}
	}
	if v.StartupScript.Env == nil {
		v.StartupScript.Env = map[string]string{}
	}
	return v
}
func (p *Provider) matches(r *Record, v backend.VM) bool {
	return r.Name == v.Name && (r.UID == "" || r.UID == v.UID) && v.UID != "" && p.Store.hash(normalized(v.Spec)) == r.NativeHash
}
func (p *Provider) Reconcile(ctx context.Context) error {
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return e
	}
	defer p.Store.mu.Unlock()
	defer p.prunePreparation()
	if p.Store.broken {
		return p.Store.stateError()
	}
	if err := p.Store.refresh(); err != nil {
		return err
	}
	vms, e := p.Backend.List(ctx)
	if e != nil {
		return backendError("Reconcile inventory", e)
	}
	if e = p.reconcileRecords(ctx, vms); e != nil {
		return e
	}

	if e := p.retireObsoleteBuilders(ctx, vms); e != nil {
		return e
	}
	if e := p.reconcileImages(ctx); e != nil {
		return e
	}
	if e := p.reconcileSlots(ctx); e != nil {
		return e
	}
	return p.reconcilePairs(ctx)
}

// Rebuild job occupancy from current native observations. In VM-properties
// mode deleted resources disappear from the index, including uncertain creates.
func (p *Provider) reconcileRecords(ctx context.Context, vms []backend.VM) error {
	var e error
	byName := map[string]backend.VM{}
	for _, v := range vms {
		byName[v.Name] = v
	}
	for _, r := range p.Store.data.Records {
		if r.PairID != "" {
			continue
		}
		if r.Phase == "done" {
			continue
		}
		v, ok := byName[r.Name]
		if !ok {
			if r.Outcome == compute.Accepted {
				r.Phase = "done"
			}
			continue
		}
		if !p.matches(r, v) {
			r.Phase = "quarantine"
			continue
		}
		r.UID = v.UID
		r.Outcome = compute.Accepted
		r.diagnostic = ""
		if (v.Status == "failed" && v.Stopped()) || r.Phase == "deleting" {
			r.Phase = "deleting"
			if e = p.Store.save(); e != nil {
				return e
			}
			// Recheck identity immediately before deletion; the scope requires exclusive name mutation.
			current, e := p.Backend.Get(ctx, r.Name)
			if backend.IsStatus(e, 404) {
				r.Phase = "done"
				continue
			}
			if e != nil {
				return backendError("Reconcile verify deletion identity", e)
			}
			if !p.matches(r, current) {
				r.Phase = "quarantine"
				continue
			}
			if !current.Stopped() {
				continue
			}
			if e = p.Backend.Delete(ctx, r.Name); e != nil && !backend.IsStatus(e, 404) {
				return backendError("Reconcile Delete", e)
			}
			_, e = p.Backend.Get(ctx, r.Name)
			if backend.IsStatus(e, 404) {
				r.Phase = "done"
			} else if e != nil {
				return backendError("Reconcile confirm deletion", e)
			}
		} else {
			r.Phase = "active"
		}
	}
	if e = p.Store.save(); e != nil {
		return e
	}
	return nil
}
func (p *Provider) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	if q.PageSize == 0 {
		q.PageSize = 100
	}
	if q.PageSize < 1 || q.PageSize > 100 || len(q.PageToken) > 16384 {
		return compute.ListResponse{}, errors.New("invalid list request")
	}
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return compute.ListResponse{}, e
	}
	defer p.Store.mu.Unlock()
	if p.Store.broken {
		return compute.ListResponse{}, p.Store.stateError()
	}
	if err := p.Store.refresh(); err != nil {
		return compute.ListResponse{}, err
	}
	after := ""
	if q.PageToken != "" {
		var c struct{ Scope, Launch, After, MAC string }
		b, e := base64.RawURLEncoding.DecodeString(q.PageToken)
		if e != nil || json.Unmarshal(b, &c) != nil || c.Scope != p.Config.Scope() || c.Launch != q.LaunchID || c.MAC != p.Store.hash([]string{c.Scope, c.Launch, c.After}) {
			return compute.ListResponse{}, errors.New("invalid cursor")
		}
		after = c.After
	}
	vs, e := p.Backend.List(ctx)
	if e != nil {
		return compute.ListResponse{}, backendError("List inventory", e)
	}
	records := map[string]*Record{}
	for _, r := range p.Store.data.Records {
		if r.PairID == "" {
			records[r.Name] = r
		}
	}
	items, err := p.pairInstances(vs, q, after)
	if err != nil {
		return compute.ListResponse{}, err
	}
	for _, v := range vs {
		r := records[v.Name]
		if r == nil || r.Phase == "done" {
			continue
		}
		if !p.matches(r, v) {
			return compute.ListResponse{}, errors.New("owned instance identity mismatch")
		}
		if v.Status == "failed" && v.Stopped() {
			continue
		}
		state := "starting"
		if v.Status == "pending" {
			state = "queued"
		}
		if v.Status == "running" || (v.Status == "failed" && !v.Stopped()) {
			state = "running"
		}
		if v.PowerState == "suspended" {
			state = "paused"
		}
		if r.Phase == "deleting" {
			state = "stopping"
		}
		if r.Name <= after || (q.LaunchID != "" && r.LaunchID != q.LaunchID) {
			continue
		}
		created := r.Created
		item := compute.Instance{ID: r.Name, LaunchID: r.LaunchID, State: state, Job: r.Job, Refs: []string{p.Config.Kind + ":" + r.Name}, CreatedAt: &created}
		if !v.StartedAt.IsZero() {
			t := v.StartedAt
			item.StartedAt = &t
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	out := compute.ListResponse{Items: items}
	if len(items) > q.PageSize {
		out.Items = items[:q.PageSize]
		after = out.Items[len(out.Items)-1].ID
		scope := p.Config.Scope()
		c := struct{ Scope, Launch, After, MAC string }{scope, q.LaunchID, after, p.Store.hash([]string{scope, q.LaunchID, after})}
		b, _ := json.Marshal(c)
		out.NextPageToken = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}
