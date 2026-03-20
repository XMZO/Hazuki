package adaptmodel

import "testing"

func TestP2QuantileTracksUpperTail(t *testing.T) {
	q := NewP2Quantile(0.90)
	for i := 1; i <= 100; i++ {
		q.Observe(float64(i))
	}
	got := q.Estimate(0)
	if got < 85 || got > 95 {
		t.Fatalf("p90 estimate = %.2f, want within [85,95]", got)
	}
}

func TestLinearRLSLearnsLine(t *testing.T) {
	m := NewLinearRLS(0, 0, 0.99, 64)
	for i := 0; i < 64; i++ {
		x := float64(i) / 8
		y := 3 + 2*x
		m.Observe(x, y)
	}

	snap := m.Snapshot()
	if snap.Samples != 64 {
		t.Fatalf("samples = %d, want 64", snap.Samples)
	}
	if snap.Intercept < 2.5 || snap.Intercept > 3.5 {
		t.Fatalf("intercept = %.4f, want near 3", snap.Intercept)
	}
	if snap.Slope < 1.8 || snap.Slope > 2.2 {
		t.Fatalf("slope = %.4f, want near 2", snap.Slope)
	}
	if got := m.Predict(10); got < 22.5 || got > 23.5 {
		t.Fatalf("predict(10) = %.4f, want near 23", got)
	}
}
