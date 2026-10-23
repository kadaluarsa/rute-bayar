package router

import "testing"

// The shipped example must always load and validate.
func TestShippedRoutingExampleLoads(t *testing.T) {
	cfg, err := LoadConfig("../../deploy/config/routing.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 5 || cfg.LatencyBudget.Milliseconds() != 3000 || cfg.Breaker.FailureThreshold != 3 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}
