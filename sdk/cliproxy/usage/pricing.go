package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// PriceCard contains per-token prices in USD. Prices are deliberately stored
// per token, matching LiteLLM and avoiding a hidden 1M-token assumption.
type PriceCard struct {
	InputPerToken       float64            `json:"input_per_token" yaml:"input-per-token"`
	OutputPerToken      float64            `json:"output_per_token" yaml:"output-per-token"`
	CacheReadPerToken   float64            `json:"cache_read_per_token" yaml:"cache-read-per-token"`
	CacheWritePerToken  float64            `json:"cache_write_per_token" yaml:"cache-write-per-token"`
	ServiceTier         map[string]float64 `json:"service_tier,omitempty" yaml:"service-tier,omitempty"`
	ReasoningMultiplier map[string]float64 `json:"reasoning_multiplier,omitempty" yaml:"reasoning-multiplier,omitempty"`
	LongContext         *LongContextPrice  `json:"long_context,omitempty" yaml:"long-context,omitempty"`
}

// LongContextPrice applies once the complete input context reaches Threshold.
// Cache read/write tokens are input-side tokens for this purpose.
type LongContextPrice struct {
	Threshold        int64   `json:"threshold" yaml:"threshold"`
	InputMultiplier  float64 `json:"input_multiplier" yaml:"input-multiplier"`
	OutputMultiplier float64 `json:"output_multiplier" yaml:"output-multiplier"`
	Inclusive        bool    `json:"inclusive" yaml:"inclusive"`
}

// PriceRule matches a model exactly or by a trailing '*'. The longest match
// wins, making specific overrides safe alongside family defaults.
type PriceRule struct {
	Model     string `json:"model" yaml:"model"`
	PriceCard `json:",inline" yaml:",inline"`
}

// PricingTable is immutable after construction. Revision is copied into every
// quote so historical charges remain auditable after a price update.
type PricingTable struct {
	Revision string      `json:"revision" yaml:"revision"`
	Default  *PriceCard  `json:"default,omitempty" yaml:"default,omitempty"`
	Rules    []PriceRule `json:"rules,omitempty" yaml:"rules,omitempty"`
}

func (t PricingTable) Resolve(model string) (PriceCard, string, bool) {
	model = strings.TrimSpace(model)
	best := -1
	bestLen := -1
	for i, rule := range t.Rules {
		pattern := strings.TrimSpace(rule.Model)
		if pattern == "" {
			continue
		}
		matched := strings.EqualFold(pattern, model)
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(strings.ToLower(model), strings.ToLower(prefix)) {
			matched = true
		}
		if matched && len(pattern) > bestLen {
			best, bestLen = i, len(pattern)
		}
	}
	if best >= 0 {
		return t.Rules[best].PriceCard, "rule:" + strings.TrimSpace(t.Rules[best].Model), true
	}
	if t.Default != nil {
		return *t.Default, "default", true
	}
	return PriceCard{}, "", false
}

// Validate rejects ambiguous pricing tables before they can be enabled. A
// zero per-token price is allowed (for a deliberately free bucket), while
// multipliers must be positive so a typo cannot silently disable billing.
func (t PricingTable) Validate() error {
	if strings.TrimSpace(t.Revision) == "" {
		return errors.New("pricing revision is empty")
	}
	if t.Default != nil {
		if err := t.Default.validate("default"); err != nil {
			return err
		}
	}
	for i, rule := range t.Rules {
		if strings.TrimSpace(rule.Model) == "" {
			return fmt.Errorf("pricing rule %d has empty model", i)
		}
		if err := rule.PriceCard.validate("rule:" + rule.Model); err != nil {
			return err
		}
	}
	if t.Default == nil && len(t.Rules) == 0 {
		return errors.New("pricing table has no default or rules")
	}
	return nil
}

func (p PriceCard) validate(name string) error {
	prices := map[string]float64{"input": p.InputPerToken, "output": p.OutputPerToken, "cache_read": p.CacheReadPerToken, "cache_write": p.CacheWritePerToken}
	for field, value := range prices {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("%s %s price is invalid", name, field)
		}
	}
	for kind, values := range map[string]map[string]float64{"service tier": p.ServiceTier, "reasoning": p.ReasoningMultiplier} {
		for key, value := range values {
			if strings.TrimSpace(key) == "" || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
				return fmt.Errorf("%s multiplier %q is invalid", name+" "+kind, key)
			}
		}
	}
	if p.LongContext != nil {
		lc := p.LongContext
		if lc.Threshold <= 0 || math.IsNaN(lc.InputMultiplier) || math.IsInf(lc.InputMultiplier, 0) || lc.InputMultiplier <= 0 || math.IsNaN(lc.OutputMultiplier) || math.IsInf(lc.OutputMultiplier, 0) || lc.OutputMultiplier <= 0 {
			return fmt.Errorf("%s long context multiplier is invalid", name)
		}
	}
	return nil
}

// TokenQuote is the auditable result of one usage calculation. Costs are USD;
// micros are rounded independently and are suitable for integer ledger storage.
type TokenQuote struct {
	Model              string                 `json:"model"`
	PriceSource        string                 `json:"price_source"`
	PriceRevision      string                 `json:"price_revision,omitempty"`
	InputTokens        int64                  `json:"input_tokens"`
	OutputTokens       int64                  `json:"output_tokens"`
	CacheReadTokens    int64                  `json:"cache_read_tokens"`
	CacheWriteTokens   int64                  `json:"cache_write_tokens"`
	InputCost          float64                `json:"input_cost"`
	OutputCost         float64                `json:"output_cost"`
	CacheReadCost      float64                `json:"cache_read_cost"`
	CacheWriteCost     float64                `json:"cache_write_cost"`
	TotalUSD           float64                `json:"total_usd"`
	TotalMicros        int64                  `json:"total_micros"`
	RateMultiplier     float64                `json:"rate_multiplier"`
	ServiceTier        string                 `json:"service_tier,omitempty"`
	ReasoningEffort    string                 `json:"reasoning_effort,omitempty"`
	LongContextApplied bool                   `json:"long_context_applied"`
	Quality            TokenAccountingQuality `json:"quality"`
}

// PriceEngine calculates quotes from a snapshot of a pricing table.
type PriceEngine struct{ table PricingTable }

func NewPriceEngine(table PricingTable) *PriceEngine {
	return &PriceEngine{table: table}
}

func (e *PriceEngine) Table() PricingTable {
	if e == nil {
		return PricingTable{}
	}
	return e.table
}

// Quote calculates non-overlapping input/output/cache costs. Unknown models
// return an error instead of silently becoming free.
func (e *PriceEngine) Quote(model string, detail Detail, rateMultiplier float64, serviceTier, reasoningEffort string) (TokenQuote, error) {
	return e.quote(model, detail, "", "", rateMultiplier, serviceTier, reasoningEffort)
}

func (e *PriceEngine) quote(model string, detail Detail, provider, executorType string, rateMultiplier float64, serviceTier, reasoningEffort string) (TokenQuote, error) {
	if e == nil {
		return TokenQuote{}, errors.New("pricing engine is nil")
	}
	model = strings.TrimSpace(model)
	card, source, ok := e.table.Resolve(model)
	if !ok {
		return TokenQuote{}, fmt.Errorf("no price configured for model %q", model)
	}
	if err := card.validate(source); err != nil {
		return TokenQuote{}, err
	}
	detail = EnsureTokenBreakdownForProvider(detail, provider, executorType)
	b := detail.TokenBreakdown
	if !b.Valid() {
		return TokenQuote{}, errors.New("invalid token breakdown")
	}
	if b.Quality != TokenAccountingQualityComplete || b.UnclassifiedTokens != 0 {
		return TokenQuote{}, fmt.Errorf("token usage is not completely classified: %s", b.Quality)
	}
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	input, output, read, write := b.Input.UncachedTokens, b.Output.TotalTokens, b.Input.CacheReadTokens, b.Input.CacheWriteTokens
	inputMul, outputMul := 1.0, 1.0
	contextTokens := b.Input.TotalTokens
	if lc := card.LongContext; lc != nil && lc.Threshold > 0 && ((lc.Inclusive && contextTokens >= lc.Threshold) || (!lc.Inclusive && contextTokens > lc.Threshold)) {
		inputMul, outputMul = positiveOrOne(lc.InputMultiplier), positiveOrOne(lc.OutputMultiplier)
	}
	tierMul := 1.0
	if m, exists := card.ServiceTier[strings.ToLower(strings.TrimSpace(serviceTier))]; exists {
		tierMul = positiveOrOne(m)
	}
	reasonMul := 1.0
	if m, exists := card.ReasoningMultiplier[strings.ToLower(strings.TrimSpace(reasoningEffort))]; exists {
		reasonMul = positiveOrOne(m)
	}
	inputCost := float64(input) * card.InputPerToken * inputMul * tierMul * rateMultiplier * reasonMul
	outputCost := float64(output) * card.OutputPerToken * outputMul * tierMul * rateMultiplier * reasonMul
	readCost := float64(read) * card.CacheReadPerToken * inputMul * tierMul * rateMultiplier * reasonMul
	writeCost := float64(write) * card.CacheWritePerToken * inputMul * tierMul * rateMultiplier * reasonMul
	total := inputCost + outputCost + readCost + writeCost
	return TokenQuote{Model: model, PriceSource: source, PriceRevision: e.table.Revision, InputTokens: input, OutputTokens: output, CacheReadTokens: read, CacheWriteTokens: write, InputCost: inputCost, OutputCost: outputCost, CacheReadCost: readCost, CacheWriteCost: writeCost, TotalUSD: total, TotalMicros: roundMicros(total), RateMultiplier: rateMultiplier, ServiceTier: serviceTier, ReasoningEffort: reasoningEffort, LongContextApplied: inputMul != 1 || outputMul != 1, Quality: b.Quality}, nil
}

func positiveOrOne(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return v
}
func roundMicros(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(math.Round(usd * 1_000_000))
}

// Charge is the boundary between pricing and a persistent billing ledger.
// Implementations must make EventID idempotent and reject a conflicting
// fingerprint for the same event.
type Charge struct {
	EventID     string     `json:"event_id"`
	RequestID   string     `json:"request_id,omitempty"`
	APIKey      string     `json:"api_key,omitempty"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	Record      Record     `json:"record"`
	Quote       TokenQuote `json:"quote"`
	CreatedAt   time.Time  `json:"created_at"`
}

type ChargeSink interface {
	Apply(context.Context, Charge) error
}

var ErrChargeConflict = errors.New("billing event fingerprint conflict")

// MemoryChargeSink is useful for development and tests. Production should
// replace it with a SQL implementation using a unique event_id and a single
// transaction for ledger insertion plus balance/quota updates.
type MemoryChargeSink struct {
	mu      sync.Mutex
	charges map[string]Charge
}

func NewMemoryChargeSink() *MemoryChargeSink {
	return &MemoryChargeSink{charges: make(map[string]Charge)}
}
func (s *MemoryChargeSink) Apply(_ context.Context, c Charge) error {
	if s == nil {
		return errors.New("charge sink is nil")
	}
	if strings.TrimSpace(c.EventID) == "" {
		return errors.New("billing event id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.charges[c.EventID]; ok {
		if old.Fingerprint != c.Fingerprint {
			return ErrChargeConflict
		}
		return nil
	}
	s.charges[c.EventID] = c
	return nil
}
func (s *MemoryChargeSink) Charges() []Charge {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Charge, 0, len(s.charges))
	for _, c := range s.charges {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out
}

// PricingPlugin converts existing usage records into charges without changing
// executor code. It is intentionally opt-in; register it only after a ledger
// and price table are configured.
type PricingPlugin struct {
	Engine         *PriceEngine
	Sink           ChargeSink
	Totals         *BillingTotals
	RateMultiplier float64
	OnError        func(error)
}

func (p *PricingPlugin) HandleUsage(ctx context.Context, record Record) {
	if p == nil || p.Engine == nil || p.Sink == nil || record.Failed || !IsClaudeProvider(record.Provider) {
		return
	}
	rate := p.RateMultiplier
	if rate <= 0 {
		rate = 1
	}
	quote, err := p.Engine.quote(record.Model, record.Detail, record.Provider, record.ExecutorType, rate, record.ServiceTier, record.ReasoningEffort)
	if err != nil {
		if p.OnError != nil {
			p.OnError(err)
		}
		return
	}
	requestID := strings.TrimSpace(record.RequestID)
	if requestID == "" {
		// Legacy records have no request ID. Derive a deterministic identity from
		// the usage fingerprint so retries with identical usage remain idempotent,
		// while a second attempt with different usage becomes a distinct event.
		requestID = "legacy:" + recordFingerprint(record)
	}
	eventID := StableBillingEventID(requestID, record)
	charge := Charge{EventID: eventID, RequestID: requestID, APIKey: record.APIKey, Fingerprint: recordFingerprint(record), Record: record, Quote: quote, CreatedAt: time.Now().UTC()}
	if err := p.Sink.Apply(ctx, charge); err != nil {
		if p.OnError != nil {
			p.OnError(err)
		}
		return
	}
	if p.Totals != nil {
		p.Totals.Add(charge)
	}
}

// IsClaudeProvider reports whether a usage record belongs to the Claude/Anthropic upstream.
// Billing is intentionally scoped to Claude; all other providers are ignored.
func IsClaudeProvider(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	return p == "claude" || p == "anthropic" || strings.Contains(p, "claude")
}

func StableBillingEventID(requestID string, record Record) string {
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(requestID)))
	h.Write([]byte{0})
	h.Write([]byte(record.AuthID))
	h.Write([]byte{0})
	h.Write([]byte(record.Provider))
	h.Write([]byte{0})
	h.Write([]byte(record.ExecutorType))
	h.Write([]byte{0})
	h.Write([]byte(record.Model))
	return hex.EncodeToString(h.Sum(nil))
}
func recordFingerprint(r Record) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d", r.Model, r.APIKey, r.Provider, r.Detail.InputTokens, r.Detail.OutputTokens, r.Detail.ReasoningTokens, r.Detail.CacheReadTokens, r.Detail.CacheCreationTokens, r.Detail.TotalTokens, r.Detail.TokenBreakdown.TotalTokens)
	return hex.EncodeToString(h.Sum(nil))
}
