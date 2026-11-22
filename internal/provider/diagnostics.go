package provider

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/colony-2/pulse-macvz/internal/config"

	api "github.com/colony-2/pulse-provider-api"
)

// Only populated under the journal lock. Observations never perform probes or
// participate in admission. Nothing here is serialized into the journal.
type diagnostics struct {
	parts     []string
	operation string
}

func (p *Provider) observe(format string, args ...any) {
	if p.diagnostic != nil {
		prefix := p.diagnostic.operation
		p.diagnostic.parts = append(p.diagnostic.parts, prefix+fmt.Sprintf(format, args...))
	}
}
func (p *Provider) observeError(operation string, err error) {
	if err != nil {
		p.observe("%s: %s", operation, err.Error())
	}
}
func backendError(operation string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrBackend, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrBackend, operation, err)
}
func (p *Provider) submission(id string, status api.Status) api.Submission {
	out := result(id, status)
	if status != api.Accepted && p.diagnostic != nil && len(p.diagnostic.parts) > 0 {
		out.Reason += ": " + strings.Join(p.diagnostic.parts, "; ")
	}
	return out
}
func combine(operation string, err error, following string, next error) error {
	var causes []error
	if err != nil {
		causes = append(causes, fmt.Errorf("%s: %w", operation, err))
	}
	if next != nil {
		causes = append(causes, fmt.Errorf("%s: %w", following, next))
	}
	return errors.Join(causes...)
}

type preparationObservation struct {
	signature string
	cause     error
	// A member identity observation can resolve only that member's failure.
	// Configuration/readiness failures require their own successful operation.
	member string
}

// Signatures include identity, configuration and phase, but never scripts.
func (p *Provider) preparationSignature(key string) string {
	if strings.HasPrefix(key, "image:") {
		if r := p.Store.data.Images[strings.TrimPrefix(key, "image:")]; r != nil {
			return p.Store.hash([]string{r.UID, r.NativeHash, r.Phase, p.Store.hash(p.Config.Config)})
		}
	}
	if strings.HasPrefix(key, "image:") {
		name := strings.TrimPrefix(key, "image:")
		if base, ok := p.requiredBaseImages()[name]; ok {
			return p.Store.hash([]string{"registry", base.Image, p.Store.hash(p.Config.Config)})
		}
	}
	if strings.HasPrefix(key, "slot:") {
		if r := p.Store.data.Slots[strings.TrimPrefix(key, "slot:")]; r != nil {
			return p.Store.hash([]string{r.UID, r.NativeHash, r.ConfigHash, r.Phase, p.Store.hash(p.Config.Config)})
		}
	}
	if strings.HasPrefix(key, "pair:") {
		if r := p.Store.data.Pairs[strings.TrimPrefix(key, "pair:")]; r != nil && r.Phase != "gone" {
			return p.Store.hash([]string{r.PrimaryUID, r.EngineUID, r.PrimaryHash, r.EngineHash, r.ConfigHash, r.Phase, p.Store.hash(p.Config.Config)})
		}
	}
	return ""
}
func (p *Provider) preparation(key string, cause error) {
	if p.preparationCauses == nil {
		p.preparationCauses = map[string]preparationObservation{}
	}
	signature := p.preparationSignature(key)
	if cause == nil || signature == "" {
		delete(p.preparationCauses, key)
		return
	}
	p.preparationCauses[key] = preparationObservation{signature: signature, cause: cause}
}

func (p *Provider) pairPreparation(pair *PairRecord, primary bool, cause error) {
	key, member := "pair:"+pair.ID, "engine"
	if primary {
		member = "primary"
	}
	if cause == nil {
		if old, ok := p.preparationCauses[key]; ok && old.member == member {
			delete(p.preparationCauses, key)
		}
		return
	}
	p.preparation(key, cause)
	if old, ok := p.preparationCauses[key]; ok {
		old.member = member
		p.preparationCauses[key] = old
	}
}
func (p *Provider) earlierPreparation(key string) {
	if old, ok := p.preparationCauses[key]; ok {
		if old.signature == p.preparationSignature(key) && old.signature != "" {
			p.observeError("earlier preparation observation", old.cause)
		} else {
			delete(p.preparationCauses, key)
		}
	}
}
func (p *Provider) prunePreparation() {
	for key, old := range p.preparationCauses {
		if old.signature != p.preparationSignature(key) {
			delete(p.preparationCauses, key)
		}
	}
}

func (p *Provider) diagnosticText() string { return strings.Join(p.diagnostic.parts, "; ") }
func (p *Provider) baseFits(b config.Base, l api.Launch) bool {
	if !matchBase(l.Base, b.Name) {
		p.observe("configured base %s does not match selector", b.Name)
		return false
	}
	for _, c := range l.Capabilities {
		if !slices.Contains(b.Capabilities, c) {
			p.observe("configured base %s lacks a requested capability", b.Name)
			return false
		}
	}
	return true
}

func unsupportedSemantics(l api.Launch) string {
	if l.TimeoutSeconds != 0 {
		return "timeout semantics unsupported"
	}
	if l.StartBefore != nil {
		return "start window semantics unsupported"
	}
	if l.ScratchBytes != 0 {
		return "scratch semantics unsupported"
	}
	return ""
}
