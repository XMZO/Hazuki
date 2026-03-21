package rewriteopt

import (
	"math"
	"testing"

	"hazuki-go/internal/proxy/rewritebudget"
)

func TestOptimizeRecoversTraceDrivenLinearPolicy(t *testing.T) {
	t.Parallel()

	result := Optimize(buildStableTrace(
		96,
		Candidate{AdmissionIntercept: 0.24, AdmissionSlope: -0.04},
		[]int64{512 << 20, 3 << 30},
	), DefaultCandidate(), 120)

	if result.BestScore.Total >= result.BaselineScore.Total {
		t.Fatalf("expected optimizer to improve training score, baseline=%f best=%f", result.BaselineScore.Total, result.BestScore.Total)
	}
	if result.Algorithm != "contextual_gaussian_process" || result.Acquisition != "expected_improvement" {
		t.Fatalf("expected contextual GP-EI metadata, got %q / %q", result.Algorithm, result.Acquisition)
	}
	if !result.Gate.Recommended {
		t.Fatalf("expected stable trace to pass the validation gate, got %q", result.Gate.Reason)
	}
	if result.Validation.Contexts < 2 {
		t.Fatalf("expected at least 2 validation contexts, got %d", result.Validation.Contexts)
	}
	if math.Abs(result.Best.AdmissionIntercept-0.24) > 0.03 {
		t.Fatalf("expected intercept near 0.240, got %.3f", result.Best.AdmissionIntercept)
	}
	if math.Abs(result.Best.AdmissionSlope-(-0.04)) > 0.05 {
		t.Fatalf("expected slope near -0.040, got %.3f", result.Best.AdmissionSlope)
	}
}

func TestOptimizeRejectsValidationDrift(t *testing.T) {
	t.Parallel()

	trace := buildDriftTrace(
		120,
		Candidate{AdmissionIntercept: 0.22, AdmissionSlope: -0.01},
		DefaultCandidate(),
		[]int64{512 << 20, 3 << 30},
	)
	result := Optimize(trace, DefaultCandidate(), 120)
	if result.Gate.Recommended {
		t.Fatalf("expected drifted trace to fail validation gate")
	}
	if result.Validation.Candidate.Total < result.Validation.Baseline.Total {
		t.Fatalf("expected challenger to be no better on validation drift, candidate=%f baseline=%f", result.Validation.Candidate.Total, result.Validation.Baseline.Total)
	}
}

func TestScoreCandidatePenalizesOverAggressivePolicy(t *testing.T) {
	t.Parallel()

	trace := []rewritebudget.RuntimeTraceRecord{
		{
			TimeUnix:                    1,
			MemoryBudgetBytes:           256 << 20,
			EffectiveUsedBytes:          210 << 20,
			BufferedAdmissionInUseBytes: 10 << 20,
			BufferedAdmissionLimitBytes: 14 << 20,
			AdaptivePressureMilli:       850,
		},
		{
			TimeUnix:              2,
			MemoryBudgetBytes:     256 << 20,
			EffectiveUsedBytes:    240 << 20,
			AdaptivePressureMilli: 900,
			PredictedUtilMilli:    960,
			CgroupMaxEvents:       2,
		},
	}

	conservative := Candidate{AdmissionIntercept: 0.24, AdmissionSlope: -0.10}
	aggressive := Candidate{AdmissionIntercept: 0.38, AdmissionSlope: 0.02}
	if ScoreCandidate(trace, aggressive).Total <= ScoreCandidate(trace, conservative).Total {
		t.Fatalf("expected aggressive policy to score worse on hot trace")
	}
}

func TestGaussianProcessPredictsObservedPoint(t *testing.T) {
	t.Parallel()

	samples := []scoredCandidate{
		{
			Candidate: Candidate{AdmissionIntercept: 0.22, AdmissionSlope: -0.18},
			Score:     Score{Total: 0.18},
		},
		{
			Candidate: Candidate{AdmissionIntercept: 0.30, AdmissionSlope: -0.09},
			Score:     Score{Total: 0.06},
		},
		{
			Candidate: Candidate{AdmissionIntercept: 0.36, AdmissionSlope: -0.03},
			Score:     Score{Total: 0.15},
		},
	}

	model := fitGaussianProcess(samples)
	mean, std := model.predict(samples[1].Candidate)
	if math.Abs(mean-samples[1].Score.Total) > 0.02 {
		t.Fatalf("expected GP mean near observed score, got mean=%.4f want=%.4f", mean, samples[1].Score.Total)
	}
	if std > 0.03 {
		t.Fatalf("expected low posterior std at observed point, got %.4f", std)
	}
}

func buildStableTrace(samples int, policy Candidate, budgets []int64) []rewritebudget.RuntimeTraceRecord {
	trace := make([]rewritebudget.RuntimeTraceRecord, 0, samples)
	for i := 0; i < samples; i++ {
		budgetBytes := budgets[i%len(budgets)]
		pressure := float64(i%10) / 9
		trace = append(trace, synthRecord(int64(i+1), budgetBytes, pressure, policy, 0))
	}
	return trace
}

func buildDriftTrace(samples int, early, late Candidate, budgets []int64) []rewritebudget.RuntimeTraceRecord {
	trace := make([]rewritebudget.RuntimeTraceRecord, 0, samples)
	split := int(math.Round(float64(samples) * 0.80))
	for i := 0; i < samples; i++ {
		policy := early
		if i >= split {
			policy = late
		}
		budgetBytes := budgets[i%len(budgets)]
		pressure := float64(i%10) / 9
		trace = append(trace, synthRecord(int64(i+1), budgetBytes, pressure, policy, 0))
	}
	return trace
}

func synthRecord(ts, budgetBytes int64, pressure float64, policy Candidate, maxEvents int64) rewritebudget.RuntimeTraceRecord {
	usedBytes := int64(float64(budgetBytes) * 0.76)
	headroomBytes := budgetBytes - usedBytes
	targetShare := clampFloat64(policy.AdmissionIntercept+policy.AdmissionSlope*pressure, minAdmissionIntercept, maxAdmissionIntercept)
	loadShare := clampFloat64(targetShare/1.08, 0, 0.95)
	inUseBytes := int64(float64(headroomBytes) * loadShare)
	limitBytes := int64(float64(headroomBytes) * targetShare)
	return rewritebudget.RuntimeTraceRecord{
		TimeUnix:                    ts,
		MemoryBudgetBytes:           budgetBytes,
		EffectiveUsedBytes:          usedBytes,
		BufferedAdmissionInUseBytes: inUseBytes,
		BufferedAdmissionLimitBytes: limitBytes,
		AdaptivePressureMilli:       int(math.Round(pressure * 1000)),
		PredictedUtilMilli:          int(math.Round((0.80 + 0.08*pressure) * 1000)),
		CgroupMaxEvents:             maxEvents,
	}
}
