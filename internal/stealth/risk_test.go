package stealth

import (
	"strings"
	"testing"
	"time"
)

// TestRiskEngineEmpty guards the no-data contract: an engine with no samples
// reports 0/low and never panics.
func TestRiskEngineEmpty(t *testing.T) {
	e := NewRiskEngine()
	st := e.Score()
	if st.Score != 0 || st.Level != RiskLow {
		t.Errorf("empty Score = %+v, want 0/low", st)
	}
	if len(st.Reasons) != 0 || st.Samples != 0 {
		t.Errorf("empty engine reasons/samples = %v/%d, want none", st.Reasons, st.Samples)
	}
	if DefaultRiskEngine.Score().Level != RiskLow {
		t.Error("DefaultRiskEngine must be safe to score with no observations")
	}
}

// TestRiskEnginePrivacySignals guards rule 1: a single upstream egress-class
// privacy signal (vpn/proxy/tor/hosting/...) is already high-risk; more
// signals add bounded weight, and the reason names them.
func TestRiskEnginePrivacySignals(t *testing.T) {
	t.Run("single tor signal is high", func(t *testing.T) {
		e := NewRiskEngine()
		e.Observe(RiskSample{IPPrivacySignals: []string{"tor"}})
		st := e.Score()
		if st.Score != 40 {
			t.Errorf("Score = %d, want 40 (egress-class floor)", st.Score)
		}
		if st.Level != RiskHigh {
			t.Errorf("Level = %q, want high (egress proxy/tor/hosting = high)", st.Level)
		}
		if len(st.Reasons) != 1 || !strings.Contains(st.Reasons[0], "tor") {
			t.Errorf("Reasons = %v, want one reason naming tor", st.Reasons)
		}
		if st.Samples != 1 {
			t.Errorf("Samples = %d, want 1", st.Samples)
		}
	})

	t.Run("multiple signals add capped weight", func(t *testing.T) {
		e := NewRiskEngine()
		e.Observe(RiskSample{
			At:               time.Now(),
			IPPrivacySignals: []string{"VPN", "proxy", "hosting"},
		})
		st := e.Score()
		if st.Score != 60 { // 40 egress-class floor + 10 each extra (capped)
			t.Errorf("Score = %d, want 60 (3 signals)", st.Score)
		}
		if st.Level != RiskHigh {
			t.Errorf("Level = %q, want high", st.Level)
		}
		if len(st.Reasons) != 1 || !strings.Contains(st.Reasons[0], "vpn") || !strings.Contains(st.Reasons[0], "proxy") {
			t.Errorf("Reasons = %v, want one reason naming vpn/proxy", st.Reasons)
		}
	})
}

// TestRiskEngineIPCap guards rule 2: activeUsersForIp/limit proximity to the
// ceiling adds score, and ratios below the thresholds add none.
func TestRiskEngineIPCap(t *testing.T) {
	t.Run("near ceiling is medium", func(t *testing.T) {
		e := NewRiskEngine()
		e.Observe(RiskSample{ActiveUsersForIP: 8, Limit: 10})
		st := e.Score()
		if st.Score != 30 {
			t.Errorf("Score = %d, want 30", st.Score)
		}
		if st.Level != RiskMedium {
			t.Errorf("Level = %q, want medium", st.Level)
		}
		if len(st.Reasons) != 1 || !strings.Contains(st.Reasons[0], "session cap") {
			t.Errorf("Reasons = %v, want the near-cap reason", st.Reasons)
		}
	})

	t.Run("signals plus cap stack to high", func(t *testing.T) {
		e := NewRiskEngine()
		e.Observe(RiskSample{
			IPPrivacySignals: []string{"vpn"},
			ActiveUsersForIP: 9,
			Limit:            10,
		})
		st := e.Score()
		if st.Score != 70 { // 40 signal floor + 30 near-cap
			t.Errorf("Score = %d, want 70", st.Score)
		}
		if st.Level != RiskHigh {
			t.Errorf("Level = %q, want high", st.Level)
		}
		if len(st.Reasons) != 2 {
			t.Errorf("Reasons = %v, want both drivers", st.Reasons)
		}
	})

	t.Run("moderate and low thresholds", func(t *testing.T) {
		e := NewRiskEngine()
		e.Observe(RiskSample{ActiveUsersForIP: 6, Limit: 10}) // 0.6 → 20
		if st := e.Score(); st.Score != 20 {
			t.Errorf("0.6 ratio Score = %d, want 20", st.Score)
		}
		e2 := NewRiskEngine()
		e2.Observe(RiskSample{ActiveUsersForIP: 4, Limit: 10}) // 0.4 → 10
		if st := e2.Score(); st.Score != 10 {
			t.Errorf("0.4 ratio Score = %d, want 10", st.Score)
		}
		e3 := NewRiskEngine()
		e3.Observe(RiskSample{ActiveUsersForIP: 2, Limit: 10}) // 0.2 → 0
		if st := e3.Score(); st.Score != 0 {
			t.Errorf("0.2 ratio Score = %d, want 0", st.Score)
		}
		e4 := NewRiskEngine()
		e4.Observe(RiskSample{ActiveUsersForIP: 0, Limit: 10}) // no active users → 0
		if st := e4.Score(); st.Score != 0 {
			t.Errorf("zero active users Score = %d, want 0", st.Score)
		}
	})

	t.Run("signal flood caps at 100", func(t *testing.T) {
		e := NewRiskEngine()
		e.Observe(RiskSample{
			IPPrivacySignals: []string{"vpn", "proxy", "tor", "hosting", "datacenter", "res_proxy"},
			ActiveUsersForIP: 10,
			Limit:            10,
		})
		st := e.Score()
		if st.Score != 90 { // 40 floor + 20 extra cap + 30 near-cap
			t.Errorf("Score = %d, want 90", st.Score)
		}
		if st.Level != RiskHigh {
			t.Errorf("Level = %q, want high", st.Level)
		}
	})
}

// TestRiskEngineObserveBounds guards the ring buffer: only the newest
// maxRiskSamples observations are retained, and the engine stays live under
// continuous observation.
func TestRiskEngineObserveBounds(t *testing.T) {
	e := NewRiskEngine()
	for i := 0; i < maxRiskSamples+10; i++ {
		e.Observe(RiskSample{ActiveUsersForIP: 9, Limit: 10})
	}
	st := e.Score()
	if st.Samples != maxRiskSamples {
		t.Errorf("Samples = %d, want capped at %d", st.Samples, maxRiskSamples)
	}
	if st.Score != 30 {
		t.Errorf("Score = %d, want 30 after heavy observation", st.Score)
	}
	e.Reset()
	if st := e.Score(); st.Samples != 0 || st.Score != 0 {
		t.Errorf("after Reset: %+v, want empty", st)
	}
}

// TestRiskEngineConcurrent guards thread safety: Observe/Score from many
// goroutines must not race or panic (the engine backs a package singleton
// fed by every token's session responses and the egress loop).
func TestRiskEngineConcurrent(t *testing.T) {
	e := NewRiskEngine()
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				e.Observe(RiskSample{ActiveUsersForIP: 5, Limit: 10, IPPrivacySignals: []string{"proxy"}})
				_ = e.Score()
			}
		}()
	}
	for range 8 {
		<-done
	}
	if st := e.Score(); st.Score < 0 || st.Score > 100 {
		t.Errorf("Score out of range after concurrent use: %+v", st)
	}
}
