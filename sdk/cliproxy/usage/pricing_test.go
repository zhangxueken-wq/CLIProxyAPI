package usage

import (
	"context"
	"testing"
)

func TestPricingTableLongestRuleWins(t *testing.T) {
	table := PricingTable{Default: &PriceCard{InputPerToken: 1}, Rules: []PriceRule{
		{Model: "gpt-*", PriceCard: PriceCard{InputPerToken: 2}},
		{Model: "gpt-5.4", PriceCard: PriceCard{InputPerToken: 3}},
	}}
	card, source, ok := table.Resolve("gpt-5.4")
	if !ok || source != "rule:gpt-5.4" || card.InputPerToken != 3 {
		t.Fatalf("resolve = %+v, %q, %v", card, source, ok)
	}
}

func TestPriceEngineQuotesMutuallyExclusiveBuckets(t *testing.T) {
	engine := NewPriceEngine(PricingTable{Revision: "r1", Default: &PriceCard{
		InputPerToken: 0.000001, OutputPerToken: 0.000002,
		CacheReadPerToken: 0.0000001, CacheWritePerToken: 0.000003,
	}})
	detail := Detail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 10, TotalTokens: 120,
		TokenBreakdown: NewSubsetTokenBreakdown(100, 30, 10, 20, 0, 120)}
	q, err := engine.Quote("model", detail, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if q.InputTokens != 60 || q.OutputTokens != 20 || q.CacheReadTokens != 30 || q.CacheWriteTokens != 10 {
		t.Fatalf("unexpected buckets: %+v", q)
	}
	want := (60*0.000001 + 20*0.000002 + 30*0.0000001 + 10*0.000003) * 2
	if q.TotalUSD != want {
		t.Fatalf("total = %.12f, want %.12f", q.TotalUSD, want)
	}
	if q.TotalMicros != int64(want*1_000_000+0.5) {
		t.Fatalf("micros = %d", q.TotalMicros)
	}
}

func TestPriceEngineLongContextAndTier(t *testing.T) {
	engine := NewPriceEngine(PricingTable{Default: &PriceCard{
		InputPerToken: 1, OutputPerToken: 1,
		ServiceTier: map[string]float64{"priority": 2},
		LongContext: &LongContextPrice{Threshold: 100, InputMultiplier: 3, OutputMultiplier: 4},
	}})
	q, err := engine.Quote("model", Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 110,
		TokenBreakdown: NewSubsetTokenBreakdown(100, 0, 0, 10, 0, 110)}, 1, "priority", "")
	if err != nil {
		t.Fatal(err)
	}
	if q.TotalUSD != 680 {
		t.Fatalf("total = %v, want 680", q.TotalUSD)
	}
	if !q.LongContextApplied {
		t.Fatal("long context flag is false")
	}
}

func TestMemoryChargeSinkIsIdempotentAndDetectsConflict(t *testing.T) {
	sink := NewMemoryChargeSink()
	c := Charge{EventID: "e1", Fingerprint: "f1"}
	if err := sink.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := sink.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := sink.Apply(context.Background(), Charge{EventID: "e1", Fingerprint: "f2"}); err != ErrChargeConflict {
		t.Fatalf("conflict error = %v", err)
	}
	if len(sink.Charges()) != 1 {
		t.Fatalf("charges = %d", len(sink.Charges()))
	}
}
