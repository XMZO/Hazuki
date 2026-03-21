package rewriteopt

import "testing"

func BenchmarkOptimizeRuntimeSized(b *testing.B) {
	cases := []struct {
		name       string
		samples    int
		iterations int
	}{
		{name: "trace512_iter48", samples: 512, iterations: 48},
		{name: "trace256_iter24", samples: 256, iterations: 24},
		{name: "trace192_iter16", samples: 192, iterations: 16},
	}

	for _, tc := range cases {
		trace := buildStableTrace(
			tc.samples,
			Candidate{AdmissionIntercept: 0.24, AdmissionSlope: -0.04},
			[]int64{512 << 20, 768 << 20, 3 << 30},
		)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = Optimize(trace, DefaultCandidate(), tc.iterations)
			}
		})
	}
}
