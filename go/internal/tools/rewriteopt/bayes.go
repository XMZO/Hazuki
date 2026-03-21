package rewriteopt

import (
	"math"
	"math/rand"
)

const (
	gpSignalVariance      = 1.0
	gpBaseNoiseVariance   = 1e-6
	gpInterceptLength     = 0.22
	gpSlopeLength         = 0.24
	boProposalGrid        = 11
	boProposalRandomCount = 144
)

type gpModel struct {
	x             [][2]float64
	alpha         []float64
	lower         [][]float64
	meanY         float64
	stdY          float64
	noiseVariance float64
	valid         bool
}

type contextGP struct {
	Key    string
	Weight float64
	Model  gpModel
}

type contextualSurrogate struct {
	Global   gpModel
	Contexts []contextGP
}

func proposeCandidate(samples []scoredCandidate, best, baseline Candidate, contextWeights map[string]float64, rng *rand.Rand, iter int) Candidate {
	if len(samples) == 0 {
		return baseline
	}

	surrogate := fitContextualSurrogate(samples, contextWeights)
	bestObserved := observedBestScore(samples)
	bestCandidate := best
	bestAcquisition := -1.0
	bestMean := math.Inf(1)

	for _, candidate := range generateProposalCandidates(best, baseline, rng, iter) {
		if minCandidateDistance(samples, candidate) < 1e-6 {
			continue
		}

		mean, std := surrogate.predict(candidate)
		ei := expectedImprovement(bestObserved, mean, std, 0.01*maxFloat64(surrogate.Global.stdY, 1e-3))
		if ei > bestAcquisition || (almostEqualFloat64(ei, bestAcquisition) && mean < bestMean) {
			bestAcquisition = ei
			bestMean = mean
			bestCandidate = candidate
		}
	}

	if bestAcquisition <= 0 {
		return randomCandidate(best, baseline, rng, iter)
	}
	return bestCandidate
}

func fitContextualSurrogate(samples []scoredCandidate, weights map[string]float64) contextualSurrogate {
	surrogate := contextualSurrogate{
		Global: fitGaussianProcess(samples),
	}

	for key, weight := range normalizeContextWeights(weights) {
		points := make([]scoredCandidate, 0, len(samples))
		for _, sample := range samples {
			score, ok := sample.ContextScores[key]
			if !ok || score.Count <= 0 {
				continue
			}
			points = append(points, scoredCandidate{
				Candidate: sample.Candidate,
				Score:     score,
			})
		}
		if len(points) == 0 {
			continue
		}
		surrogate.Contexts = append(surrogate.Contexts, contextGP{
			Key:    key,
			Weight: weight,
			Model:  fitGaussianProcess(points),
		})
	}
	return surrogate
}

func normalizeContextWeights(weights map[string]float64) map[string]float64 {
	if len(weights) == 0 {
		return nil
	}
	total := 0.0
	out := make(map[string]float64, len(weights))
	for key, weight := range weights {
		if weight <= 0 {
			continue
		}
		out[key] = weight
		total += weight
	}
	if total <= 0 {
		return nil
	}
	for key, weight := range out {
		out[key] = weight / total
	}
	return out
}

func fitGaussianProcess(samples []scoredCandidate) gpModel {
	if len(samples) == 0 {
		return gpModel{}
	}

	model := gpModel{
		x:     make([][2]float64, len(samples)),
		alpha: make([]float64, len(samples)),
	}

	ys := make([]float64, len(samples))
	for i, sample := range samples {
		model.x[i] = normalizeCandidate(sample.Candidate)
		ys[i] = sample.Score.Total
		model.meanY += ys[i]
	}
	model.meanY /= float64(len(ys))

	for _, y := range ys {
		delta := y - model.meanY
		model.stdY += delta * delta
	}
	model.stdY = math.Sqrt(model.stdY / float64(len(ys)))
	if model.stdY < 1e-9 {
		model.stdY = 1
	}

	yNorm := make([]float64, len(ys))
	for i, y := range ys {
		yNorm[i] = (y - model.meanY) / model.stdY
	}

	for _, jitter := range []float64{gpBaseNoiseVariance, 1e-5, 1e-4, 1e-3} {
		k := make([][]float64, len(samples))
		for i := range k {
			k[i] = make([]float64, len(samples))
		}
		for i := range samples {
			for j := 0; j <= i; j++ {
				value := gpKernel(model.x[i], model.x[j])
				if i == j {
					value += jitter
				}
				k[i][j] = value
				k[j][i] = value
			}
		}

		lower, ok := choleskyDecompose(k)
		if !ok {
			continue
		}
		alpha := solveSymmetricFromCholesky(lower, yNorm)
		model.lower = lower
		model.alpha = alpha
		model.noiseVariance = jitter
		model.valid = true
		return model
	}

	return gpModel{
		meanY: model.meanY,
		stdY:  model.stdY,
	}
}

func (m gpModel) predict(candidate Candidate) (float64, float64) {
	if !m.valid || len(m.x) == 0 {
		return m.meanY, maxFloat64(m.stdY, 1e-3)
	}

	x := normalizeCandidate(candidate)
	k := make([]float64, len(m.x))
	for i := range m.x {
		k[i] = gpKernel(x, m.x[i])
	}

	meanNorm := 0.0
	for i := range k {
		meanNorm += k[i] * m.alpha[i]
	}

	v := forwardSubstitute(m.lower, k)
	varianceNorm := gpKernel(x, x)
	for _, value := range v {
		varianceNorm -= value * value
	}
	if varianceNorm < 1e-9 {
		varianceNorm = 1e-9
	}

	return meanNorm*m.stdY + m.meanY, math.Sqrt(varianceNorm) * m.stdY
}

func (s contextualSurrogate) predict(candidate Candidate) (float64, float64) {
	if len(s.Contexts) == 0 {
		return s.Global.predict(candidate)
	}

	mean := 0.0
	variance := 0.0
	totalWeight := 0.0
	for _, ctx := range s.Contexts {
		ctxMean, ctxStd := ctx.Model.predict(candidate)
		mean += ctx.Weight * ctxMean
		variance += (ctx.Weight * ctxStd) * (ctx.Weight * ctxStd)
		totalWeight += ctx.Weight
	}
	if totalWeight <= 0 {
		return s.Global.predict(candidate)
	}
	return mean / totalWeight, math.Sqrt(variance) / totalWeight
}

func generateProposalCandidates(best, baseline Candidate, rng *rand.Rand, iter int) []Candidate {
	candidates := make([]Candidate, 0, boProposalGrid*boProposalGrid+boProposalRandomCount)

	shiftI := rng.Float64() / float64(boProposalGrid*3)
	shiftS := rng.Float64() / float64(boProposalGrid*3)
	for i := 0; i < boProposalGrid; i++ {
		interceptRatio := clampFloat64((float64(i)+shiftI)/float64(boProposalGrid-1), 0, 1)
		intercept := minAdmissionIntercept + interceptRatio*(maxAdmissionIntercept-minAdmissionIntercept)
		for j := 0; j < boProposalGrid; j++ {
			slopeRatio := clampFloat64((float64(j)+shiftS)/float64(boProposalGrid-1), 0, 1)
			slope := minAdmissionSlope + slopeRatio*(maxAdmissionSlope-minAdmissionSlope)
			candidates = append(candidates, Candidate{
				AdmissionIntercept: intercept,
				AdmissionSlope:     slope,
			})
		}
	}

	for i := 0; i < boProposalRandomCount; i++ {
		candidates = append(candidates, randomCandidate(best, baseline, rng, iter))
	}
	return candidates
}

func randomCandidate(best, baseline Candidate, rng *rand.Rand, iter int) Candidate {
	mode := rng.Float64()
	decay := math.Exp(-0.018 * float64(iter))

	switch {
	case mode < 0.55:
		return clampCandidate(Candidate{
			AdmissionIntercept: best.AdmissionIntercept + (rng.Float64()*2-1)*(0.040*decay+0.005),
			AdmissionSlope:     best.AdmissionSlope + (rng.Float64()*2-1)*(0.080*decay+0.010),
		})
	case mode < 0.85:
		return clampCandidate(Candidate{
			AdmissionIntercept: baseline.AdmissionIntercept + (rng.Float64()*2-1)*(0.048*decay+0.008),
			AdmissionSlope:     baseline.AdmissionSlope + (rng.Float64()*2-1)*(0.096*decay+0.012),
		})
	default:
		return clampCandidate(Candidate{
			AdmissionIntercept: minAdmissionIntercept + rng.Float64()*(maxAdmissionIntercept-minAdmissionIntercept),
			AdmissionSlope:     minAdmissionSlope + rng.Float64()*(maxAdmissionSlope-minAdmissionSlope),
		})
	}
}

func observedBestScore(samples []scoredCandidate) float64 {
	best := math.Inf(1)
	for _, sample := range samples {
		if sample.Score.Total < best {
			best = sample.Score.Total
		}
	}
	if !math.IsInf(best, 1) {
		return best
	}
	return 0
}

func expectedImprovement(bestObserved, mean, std, xi float64) float64 {
	if std <= 1e-12 {
		improvement := bestObserved - mean - xi
		if improvement > 0 {
			return improvement
		}
		return 0
	}

	improvement := bestObserved - mean - xi
	z := improvement / std
	return improvement*normalCDF(z) + std*normalPDF(z)
}

func gpKernel(a, b [2]float64) float64 {
	di := (a[0] - b[0]) / gpInterceptLength
	ds := (a[1] - b[1]) / gpSlopeLength
	return gpSignalVariance * math.Exp(-0.5*(di*di+ds*ds))
}

func normalizeCandidate(candidate Candidate) [2]float64 {
	candidate = clampCandidate(candidate)
	return [2]float64{
		(candidate.AdmissionIntercept - minAdmissionIntercept) / (maxAdmissionIntercept - minAdmissionIntercept),
		(candidate.AdmissionSlope - minAdmissionSlope) / (maxAdmissionSlope - minAdmissionSlope),
	}
}

func choleskyDecompose(matrix [][]float64) ([][]float64, bool) {
	n := len(matrix)
	lower := make([][]float64, n)
	for i := range lower {
		lower[i] = make([]float64, n)
	}

	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			sum := matrix[i][j]
			for k := 0; k < j; k++ {
				sum -= lower[i][k] * lower[j][k]
			}
			if i == j {
				if sum <= 1e-12 {
					return nil, false
				}
				lower[i][j] = math.Sqrt(sum)
				continue
			}
			lower[i][j] = sum / lower[j][j]
		}
	}
	return lower, true
}

func solveSymmetricFromCholesky(lower [][]float64, b []float64) []float64 {
	return backwardSubstitute(lower, forwardSubstitute(lower, b))
}

func forwardSubstitute(lower [][]float64, b []float64) []float64 {
	out := make([]float64, len(b))
	for i := 0; i < len(lower); i++ {
		sum := b[i]
		for j := 0; j < i; j++ {
			sum -= lower[i][j] * out[j]
		}
		out[i] = sum / lower[i][i]
	}
	return out
}

func backwardSubstitute(lower [][]float64, b []float64) []float64 {
	n := len(lower)
	out := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		sum := b[i]
		for j := i + 1; j < n; j++ {
			sum -= lower[j][i] * out[j]
		}
		out[i] = sum / lower[i][i]
	}
	return out
}

func normalCDF(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

func normalPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

func almostEqualFloat64(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12
}
