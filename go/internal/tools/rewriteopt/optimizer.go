package rewriteopt

import (
	"math"
	"math/rand"
	"sort"
	"strings"

	"hazuki-go/internal/proxy/rewritebudget"
)

const (
	defaultAdmissionIntercept = 0.34
	defaultAdmissionSlope     = -0.14
	minAdmissionIntercept     = 0.16
	maxAdmissionIntercept     = 0.38
	minAdmissionSlope         = -0.30
	maxAdmissionSlope         = 0.08

	validationRatio               = 0.20
	minValidationObservations     = 12
	minValidationImprovementPct   = 2.0
	maxValidationRiskIncreasePct  = 4.0
	maxContextRegressionPct       = 8.0
	minContextObservationsForGate = 4
)

type Candidate struct {
	AdmissionIntercept float64 `json:"admissionIntercept"`
	AdmissionSlope     float64 `json:"admissionSlope"`
}

type Score struct {
	Total  float64 `json:"total"`
	Risk   float64 `json:"risk"`
	CPU    float64 `json:"cpu"`
	Jitter float64 `json:"jitter"`
	Count  int     `json:"count"`
}

type ContextComparison struct {
	Key            string  `json:"key"`
	Count          int     `json:"count"`
	Weight         float64 `json:"weight"`
	Baseline       Score   `json:"baseline"`
	Candidate      Score   `json:"candidate"`
	ImprovementPct float64 `json:"improvementPct"`
}

type DatasetSummary struct {
	Observations   int                 `json:"observations"`
	Contexts       int                 `json:"contexts"`
	Weighting      string              `json:"weighting"`
	Baseline       Score               `json:"baseline"`
	Candidate      Score               `json:"candidate"`
	ImprovementPct float64             `json:"improvementPct"`
	ContextScores  []ContextComparison `json:"contextScores"`
}

type GateDecision struct {
	Recommended                bool    `json:"recommended"`
	Reason                     string  `json:"reason"`
	TrainImprovementPct        float64 `json:"trainImprovementPct"`
	ValidationImprovementPct   float64 `json:"validationImprovementPct"`
	RequiredImprovementPct     float64 `json:"requiredImprovementPct"`
	RiskIncreasePct            float64 `json:"riskIncreasePct"`
	MaxRiskIncreasePct         float64 `json:"maxRiskIncreasePct"`
	WorstContextKey            string  `json:"worstContextKey"`
	WorstContextImprovementPct float64 `json:"worstContextImprovementPct"`
	ValidationObservations     int     `json:"validationObservations"`
	ValidationContexts         int     `json:"validationContexts"`
}

type Result struct {
	Samples            int            `json:"samples"`
	Observations       int            `json:"observations"`
	ExploredCandidates int            `json:"exploredCandidates"`
	Algorithm          string         `json:"algorithm"`
	Acquisition        string         `json:"acquisition"`
	SplitStrategy      string         `json:"splitStrategy"`
	ContextModel       string         `json:"contextModel"`
	Baseline           Candidate      `json:"baseline"`
	BaselineScore      Score          `json:"baselineScore"`
	Best               Candidate      `json:"best"`
	BestScore          Score          `json:"bestScore"`
	ImprovementPct     float64        `json:"improvementPct"`
	Train              DatasetSummary `json:"train"`
	Validation         DatasetSummary `json:"validation"`
	Gate               GateDecision   `json:"gate"`
}

type scoredCandidate struct {
	Candidate     Candidate
	Score         Score
	ContextScores map[string]Score
}

type observation struct {
	TimeUnix      int64
	Pressure      float64
	TargetShare   float64
	RiskWeight    float64
	CPUWeight     float64
	JitterWeight  float64
	ContextKey    string
	ContextBucket contextBucket
}

type contextBucket struct {
	MemoryTier string
	LoadTier   string
	HeatTier   string
}

type datasetEvaluation struct {
	Score         Score
	ContextScores map[string]Score
}

func DefaultCandidate() Candidate {
	return Candidate{
		AdmissionIntercept: defaultAdmissionIntercept,
		AdmissionSlope:     defaultAdmissionSlope,
	}
}

func (c Candidate) Share(pressure float64) float64 {
	return clampFloat64(c.AdmissionIntercept+c.AdmissionSlope*clampFloat64(pressure, 0, 1), minAdmissionIntercept, maxAdmissionIntercept)
}

func Optimize(trace []rewritebudget.RuntimeTraceRecord, baseline Candidate, iterations int) Result {
	trace = compactTrace(trace)
	if iterations <= 0 {
		iterations = 72
	}

	baseline = clampCandidate(baseline)
	observations := buildObservations(trace)
	train, validation := splitObservations(observations)
	if len(train) == 0 {
		train = observations
		validation = nil
	}

	trainWeights := contextWeights(train)
	validationWeights := contextWeights(validation)
	baselineTrain := evaluateCandidate(train, baseline, trainWeights)

	result := Result{
		Samples:       len(trace),
		Observations:  len(observations),
		Algorithm:     "contextual_gaussian_process",
		Acquisition:   "expected_improvement",
		SplitStrategy: "temporal_stratified_holdout",
		ContextModel:  "memory_tier+load_tier+heat_tier",
		Baseline:      baseline,
		Best:          baseline,
		BaselineScore: baselineTrain.Score,
		BestScore:     baselineTrain.Score,
	}
	result.Train = buildDatasetSummary(train, trainWeights, baselineTrain, baselineTrain)

	if len(train) == 0 {
		result.Validation = buildDatasetSummary(validation, validationWeights, datasetEvaluation{}, datasetEvaluation{})
		result.Gate = evaluateGate(result.Train, result.Validation)
		return result
	}

	samples := make([]scoredCandidate, 0, 128+iterations)
	seen := make(map[[2]int]struct{}, 128+iterations)
	addCandidate := func(candidate Candidate) {
		candidate = clampCandidate(candidate)
		key := candidateKey(candidate)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		eval := evaluateCandidate(train, candidate, trainWeights)
		samples = append(samples, scoredCandidate{
			Candidate:     candidate,
			Score:         eval.Score,
			ContextScores: eval.ContextScores,
		})
		if eval.Score.Total < result.BestScore.Total || result.ExploredCandidates == 0 {
			result.Best = candidate
			result.BestScore = eval.Score
		}
		result.ExploredCandidates++
	}

	for _, candidate := range seedCandidates(baseline) {
		addCandidate(candidate)
	}

	rng := rand.New(rand.NewSource(0x5A17C0DE))
	for i := 0; i < iterations; i++ {
		addCandidate(proposeCandidate(samples, result.Best, baseline, trainWeights, rng, i))
	}

	bestTrain := evaluateCandidate(train, result.Best, trainWeights)
	baselineValidation := evaluateCandidate(validation, baseline, validationWeights)
	bestValidation := evaluateCandidate(validation, result.Best, validationWeights)

	result.BaselineScore = baselineTrain.Score
	result.BestScore = bestTrain.Score
	result.ImprovementPct = improvementPct(result.BaselineScore.Total, result.BestScore.Total)
	result.Train = buildDatasetSummary(train, trainWeights, baselineTrain, bestTrain)
	result.Validation = buildDatasetSummary(validation, validationWeights, baselineValidation, bestValidation)
	result.Gate = evaluateGate(result.Train, result.Validation)
	return result
}

func ScoreCandidate(trace []rewritebudget.RuntimeTraceRecord, candidate Candidate) Score {
	observations := buildObservations(compactTrace(trace))
	return evaluateCandidate(observations, clampCandidate(candidate), contextWeights(observations)).Score
}

func buildObservations(trace []rewritebudget.RuntimeTraceRecord) []observation {
	if len(trace) < 2 {
		return nil
	}

	out := make([]observation, 0, len(trace)-1)
	for i := 1; i < len(trace); i++ {
		if obs, ok := buildObservation(trace[i-1], trace[i]); ok {
			out = append(out, obs)
		}
	}
	return out
}

func buildObservation(prev, next rewritebudget.RuntimeTraceRecord) (observation, bool) {
	headroomBytes := prev.MemoryBudgetBytes - prev.EffectiveUsedBytes
	if prev.MemoryBudgetBytes <= 0 || headroomBytes <= 0 {
		return observation{}, false
	}

	targetShare, ok := deriveObservedAdmissionShareTarget(prev, next)
	if !ok {
		return observation{}, false
	}

	loadShare := 0.0
	if prev.BufferedAdmissionInUseBytes > 0 {
		loadShare = clampFloat64(float64(prev.BufferedAdmissionInUseBytes)/float64(headroomBytes), 0, 1)
	}

	fallbackDelta := positiveDelta(next.BufferedAdmissionFallbacks, prev.BufferedAdmissionFallbacks)
	activeWeightRatio := 0.0
	if prev.ActiveRewriteWeightBytes > 0 {
		activeWeightRatio = clampFloat64(float64(prev.ActiveRewriteWeightBytes)/float64(prev.MemoryBudgetBytes), 0, 1)
	}

	loadScore := maxFloat64(loadShare, activeWeightRatio)
	hotness := hotnessScore(prev, next)
	bucket := classifyContext(prev, loadScore, hotness)
	return observation{
		TimeUnix:      maxInt64(next.TimeUnix, prev.TimeUnix),
		Pressure:      pressureFromRecord(prev),
		TargetShare:   targetShare,
		RiskWeight:    1.0 + hotness,
		CPUWeight:     0.30 + 0.90*loadShare + 0.40*activeWeightRatio + 0.08*float64(minInt64(fallbackDelta, 4)),
		JitterWeight:  0.03 + 0.04*pressureFromRecord(prev),
		ContextKey:    bucket.String(),
		ContextBucket: bucket,
	}, true
}

func classifyContext(record rewritebudget.RuntimeTraceRecord, loadScore, hotness float64) contextBucket {
	return contextBucket{
		MemoryTier: memoryTier(record.MemoryBudgetBytes),
		LoadTier:   loadTier(loadScore),
		HeatTier:   heatTier(hotness),
	}
}

func memoryTier(memoryBudgetBytes int64) string {
	switch {
	case memoryBudgetBytes <= 0:
		return "unknown"
	case memoryBudgetBytes <= 768<<20:
		return "tiny"
	case memoryBudgetBytes <= 2<<30:
		return "small"
	case memoryBudgetBytes <= 8<<30:
		return "medium"
	default:
		return "large"
	}
}

func loadTier(loadScore float64) string {
	switch {
	case loadScore < 0.05:
		return "light"
	case loadScore < 0.15:
		return "moderate"
	default:
		return "heavy"
	}
}

func heatTier(hotness float64) string {
	switch {
	case hotness < 0.25:
		return "calm"
	case hotness < 0.90:
		return "warm"
	default:
		return "hot"
	}
}

func (b contextBucket) String() string {
	var parts []string
	if b.MemoryTier != "" {
		parts = append(parts, "mem="+b.MemoryTier)
	}
	if b.LoadTier != "" {
		parts = append(parts, "load="+b.LoadTier)
	}
	if b.HeatTier != "" {
		parts = append(parts, "heat="+b.HeatTier)
	}
	return strings.Join(parts, "|")
}

func splitObservations(observations []observation) ([]observation, []observation) {
	if len(observations) == 0 {
		return nil, nil
	}

	byContext := make(map[string][]int)
	for idx, obs := range observations {
		byContext[obs.ContextKey] = append(byContext[obs.ContextKey], idx)
	}

	isValidation := make([]bool, len(observations))
	validationCount := 0
	for _, indices := range byContext {
		n := len(indices)
		if n < 3 {
			continue
		}
		want := int(math.Round(float64(n) * validationRatio))
		if want <= 0 {
			want = 1
		}
		if want >= n {
			want = n / 3
		}
		if want <= 0 {
			continue
		}
		for _, idx := range indices[n-want:] {
			isValidation[idx] = true
			validationCount++
		}
	}

	if validationCount == 0 && len(observations) >= 8 {
		want := maxInt(1, int(math.Round(float64(len(observations))*validationRatio)))
		for _, idx := range lastIndices(len(observations), want) {
			isValidation[idx] = true
			validationCount++
		}
	}

	train := make([]observation, 0, len(observations))
	validation := make([]observation, 0, validationCount)
	for idx, obs := range observations {
		if isValidation[idx] {
			validation = append(validation, obs)
			continue
		}
		train = append(train, obs)
	}

	if len(train) == 0 {
		return observations, nil
	}
	return train, validation
}

func lastIndices(size, count int) []int {
	if count <= 0 || size <= 0 {
		return nil
	}
	if count > size {
		count = size
	}
	out := make([]int, 0, count)
	for idx := size - count; idx < size; idx++ {
		out = append(out, idx)
	}
	return out
}

func contextWeights(observations []observation) map[string]float64 {
	if len(observations) == 0 {
		return nil
	}

	counts := make(map[string]int)
	for _, obs := range observations {
		counts[obs.ContextKey]++
	}

	weights := make(map[string]float64, len(counts))
	total := 0.0
	for key, count := range counts {
		weight := math.Sqrt(float64(count))
		weights[key] = weight
		total += weight
	}
	if total <= 0 {
		weight := 1.0 / float64(len(weights))
		for key := range weights {
			weights[key] = weight
		}
		return weights
	}
	for key, weight := range weights {
		weights[key] = weight / total
	}
	return weights
}

func evaluateCandidate(observations []observation, candidate Candidate, weights map[string]float64) datasetEvaluation {
	eval := datasetEvaluation{
		Score:         Score{Count: len(observations)},
		ContextScores: make(map[string]Score),
	}
	if len(observations) == 0 {
		return eval
	}

	candidate = clampCandidate(candidate)
	prevShare := 0.0
	havePrevShare := false

	for _, obs := range observations {
		share := candidate.Share(obs.Pressure)
		over := maxFloat64(share-obs.TargetShare, 0)
		under := maxFloat64(obs.TargetShare-share, 0)

		riskCost := obs.RiskWeight * huber(over, 0.025)
		cpuCost := obs.CPUWeight * huber(under, 0.030)
		jitterCost := 0.0
		if havePrevShare {
			jitterCost = obs.JitterWeight * huber(math.Abs(share-prevShare), 0.020)
		}

		score := eval.ContextScores[obs.ContextKey]
		score.Risk += riskCost
		score.CPU += cpuCost
		score.Jitter += jitterCost
		score.Total += riskCost + cpuCost + jitterCost
		score.Count++
		eval.ContextScores[obs.ContextKey] = score

		prevShare = share
		havePrevShare = true
	}

	for key, score := range eval.ContextScores {
		eval.ContextScores[key] = normalizeScore(score)
	}
	eval.Score = aggregateScore(eval.ContextScores, weights, len(observations))
	return eval
}

func normalizeScore(score Score) Score {
	if score.Count <= 0 {
		return score
	}
	inv := 1 / float64(score.Count)
	score.Total *= inv
	score.Risk *= inv
	score.CPU *= inv
	score.Jitter *= inv
	return score
}

func aggregateScore(contextScores map[string]Score, weights map[string]float64, count int) Score {
	result := Score{Count: count}
	if len(contextScores) == 0 {
		return result
	}

	normalizedWeights := normalizeWeights(weights, contextScores)
	for key, score := range contextScores {
		weight := normalizedWeights[key]
		result.Total += weight * score.Total
		result.Risk += weight * score.Risk
		result.CPU += weight * score.CPU
		result.Jitter += weight * score.Jitter
	}
	return result
}

func normalizeWeights(weights map[string]float64, contextScores map[string]Score) map[string]float64 {
	out := make(map[string]float64, len(contextScores))
	total := 0.0
	for key := range contextScores {
		weight := 0.0
		if weights != nil {
			weight = weights[key]
		}
		if weight <= 0 {
			weight = 1
		}
		out[key] = weight
		total += weight
	}
	if total <= 0 {
		return out
	}
	for key, weight := range out {
		out[key] = weight / total
	}
	return out
}

func buildDatasetSummary(observations []observation, weights map[string]float64, baseline, candidate datasetEvaluation) DatasetSummary {
	contextKeys := collectContextKeys(baseline.ContextScores, candidate.ContextScores)
	summary := DatasetSummary{
		Observations:   len(observations),
		Contexts:       len(contextKeys),
		Weighting:      "sqrt_context_balance",
		Baseline:       baseline.Score,
		Candidate:      candidate.Score,
		ImprovementPct: improvementPct(baseline.Score.Total, candidate.Score.Total),
		ContextScores:  make([]ContextComparison, 0, len(contextKeys)),
	}

	normalizedWeights := normalizeWeights(weights, mergeContextScores(contextKeys))
	for _, key := range contextKeys {
		baseScore := baseline.ContextScores[key]
		candidateScore := candidate.ContextScores[key]
		summary.ContextScores = append(summary.ContextScores, ContextComparison{
			Key:            key,
			Count:          maxInt(baseScore.Count, candidateScore.Count),
			Weight:         normalizedWeights[key],
			Baseline:       baseScore,
			Candidate:      candidateScore,
			ImprovementPct: improvementPct(baseScore.Total, candidateScore.Total),
		})
	}
	return summary
}

func collectContextKeys(maps ...map[string]Score) []string {
	keys := make(map[string]struct{})
	for _, scores := range maps {
		for key := range scores {
			keys[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func mergeContextScores(keys []string) map[string]Score {
	out := make(map[string]Score, len(keys))
	for _, key := range keys {
		out[key] = Score{Count: 1}
	}
	return out
}

func evaluateGate(train, validation DatasetSummary) GateDecision {
	gate := GateDecision{
		TrainImprovementPct:        train.ImprovementPct,
		ValidationImprovementPct:   validation.ImprovementPct,
		RequiredImprovementPct:     minValidationImprovementPct,
		MaxRiskIncreasePct:         maxValidationRiskIncreasePct,
		ValidationObservations:     validation.Observations,
		ValidationContexts:         validation.Contexts,
		WorstContextImprovementPct: 0,
	}

	if validation.Observations < minValidationObservations {
		gate.Reason = "insufficient_validation_observations"
		return gate
	}

	requiredContexts := minInt(maxInt(train.Contexts, 1), 2)
	if validation.Contexts < requiredContexts {
		gate.Reason = "insufficient_validation_contexts"
		return gate
	}

	riskDelta := validation.Candidate.Risk - validation.Baseline.Risk
	gate.RiskIncreasePct = deltaPct(validation.Baseline.Risk, validation.Candidate.Risk)

	requiredGain := maxFloat64(0.0010, validation.Baseline.Total*(minValidationImprovementPct/100))
	if validation.Baseline.Total-validation.Candidate.Total < requiredGain {
		if gate.Reason == "" {
			gate.Reason = "validation_improvement_too_small"
		}
	}

	maxRiskDelta := maxFloat64(0.0006, validation.Baseline.Risk*(maxValidationRiskIncreasePct/100))
	if riskDelta > maxRiskDelta {
		if gate.Reason == "" {
			gate.Reason = "validation_risk_regressed"
		}
	}

	worstKey, worstImprovement, worstDelta := worstContextRegression(validation.ContextScores)
	gate.WorstContextKey = worstKey
	gate.WorstContextImprovementPct = worstImprovement
	if worstDelta > 0 {
		if gate.Reason == "" {
			gate.Reason = "context_regression_too_large"
		}
	}

	if gate.Reason == "" {
		gate.Recommended = true
		gate.Reason = "promote"
	}
	return gate
}

func worstContextRegression(contexts []ContextComparison) (string, float64, float64) {
	worstKey := ""
	worstImprovement := 0.0
	worstDelta := 0.0
	fallbackKey := ""
	fallbackImprovement := 0.0
	fallbackDelta := 0.0

	for _, ctx := range contexts {
		delta := ctx.Candidate.Total - ctx.Baseline.Total
		if ctx.Count >= minContextObservationsForGate && delta > worstDelta {
			limit := maxFloat64(0.0008, ctx.Baseline.Total*(maxContextRegressionPct/100))
			if delta > limit {
				worstDelta = delta
				worstKey = ctx.Key
				worstImprovement = ctx.ImprovementPct
			}
		}
		if delta > 0 && (fallbackKey == "" || delta > fallbackDelta) {
			fallbackKey = ctx.Key
			fallbackImprovement = ctx.ImprovementPct
			fallbackDelta = delta
		}
	}

	if worstKey != "" {
		return worstKey, worstImprovement, worstDelta
	}
	return fallbackKey, fallbackImprovement, 0
}

func deriveObservedAdmissionShareTarget(prev, next rewritebudget.RuntimeTraceRecord) (float64, bool) {
	headroomBytes := prev.MemoryBudgetBytes - prev.EffectiveUsedBytes
	if headroomBytes <= 0 {
		return 0, false
	}

	loadShare := 0.0
	if prev.BufferedAdmissionInUseBytes > 0 {
		loadShare = float64(prev.BufferedAdmissionInUseBytes) / float64(headroomBytes)
	}
	if loadShare <= 0 && prev.BufferedAdmissionLimitBytes <= 0 {
		return 0, false
	}

	target := clampFloat64(maxFloat64(loadShare*1.08, 0.18), minAdmissionIntercept, maxAdmissionIntercept)
	highDelta := positiveDelta(next.CgroupHighEvents, prev.CgroupHighEvents)
	maxDelta := positiveDelta(next.CgroupMaxEvents, prev.CgroupMaxEvents)
	oomDelta := positiveDelta(next.CgroupOOMEvents, prev.CgroupOOMEvents)
	oomKillDelta := positiveDelta(next.CgroupOOMKillEvents, prev.CgroupOOMKillEvents)
	nextUtil := utilizationRatio(next)

	switch {
	case oomDelta > 0 || oomKillDelta > 0 || nextUtil >= 0.94:
		target *= 0.68
	case maxDelta > 0 || nextUtil >= 0.90:
		target *= 0.78
	case highDelta > 0 || nextUtil >= 0.84:
		target *= 0.90
	case loadShare > 0.03 && nextUtil <= 0.75:
		target *= 1.05
	}
	return clampFloat64(target, minAdmissionIntercept, maxAdmissionIntercept), true
}

func seedCandidates(baseline Candidate) []Candidate {
	defaultCandidate := DefaultCandidate()
	seeds := []Candidate{
		baseline,
		defaultCandidate,
		{AdmissionIntercept: baseline.AdmissionIntercept - 0.02, AdmissionSlope: baseline.AdmissionSlope},
		{AdmissionIntercept: baseline.AdmissionIntercept + 0.02, AdmissionSlope: baseline.AdmissionSlope},
		{AdmissionIntercept: baseline.AdmissionIntercept, AdmissionSlope: baseline.AdmissionSlope - 0.04},
		{AdmissionIntercept: baseline.AdmissionIntercept, AdmissionSlope: baseline.AdmissionSlope + 0.04},
	}

	for interceptStep := 0; interceptStep < 6; interceptStep++ {
		intercept := minAdmissionIntercept + (maxAdmissionIntercept-minAdmissionIntercept)*float64(interceptStep)/5
		for slopeStep := 0; slopeStep < 6; slopeStep++ {
			slope := minAdmissionSlope + (maxAdmissionSlope-minAdmissionSlope)*float64(slopeStep)/5
			seeds = append(seeds, Candidate{
				AdmissionIntercept: intercept,
				AdmissionSlope:     slope,
			})
		}
	}
	return seeds
}

func compactTrace(trace []rewritebudget.RuntimeTraceRecord) []rewritebudget.RuntimeTraceRecord {
	if len(trace) == 0 {
		return nil
	}

	out := make([]rewritebudget.RuntimeTraceRecord, 0, len(trace))
	for _, record := range trace {
		if record.TimeUnix == 0 && record.MemoryBudgetBytes == 0 && record.EffectiveUsedBytes == 0 {
			continue
		}
		out = append(out, record)
	}
	return out
}

func hotnessScore(prev, next rewritebudget.RuntimeTraceRecord) float64 {
	highDelta := positiveDelta(next.CgroupHighEvents, prev.CgroupHighEvents)
	maxDelta := positiveDelta(next.CgroupMaxEvents, prev.CgroupMaxEvents)
	oomDelta := positiveDelta(next.CgroupOOMEvents, prev.CgroupOOMEvents)
	oomKillDelta := positiveDelta(next.CgroupOOMKillEvents, prev.CgroupOOMKillEvents)

	hotUtil := maxFloat64(utilizationRatio(next), float64(next.PredictedUtilMilli)/1000)
	utilPenalty := 0.0
	if hotUtil > 0.82 {
		utilPenalty = math.Pow((hotUtil-0.82)/0.18, 2)
	}

	return utilPenalty +
		0.12*float64(minInt64(highDelta, 4)) +
		0.22*float64(minInt64(maxDelta, 4)) +
		1.40*float64(minInt64(oomDelta+oomKillDelta, 2))
}

func utilizationRatio(record rewritebudget.RuntimeTraceRecord) float64 {
	if record.MemoryBudgetBytes <= 0 || record.EffectiveUsedBytes <= 0 {
		return 0
	}
	return clampFloat64(float64(record.EffectiveUsedBytes)/float64(record.MemoryBudgetBytes), 0, 1.5)
}

func pressureFromRecord(record rewritebudget.RuntimeTraceRecord) float64 {
	return pressureFromRecordValue(record.AdaptivePressureMilli)
}

func pressureFromRecordValue(value int) float64 {
	return clampFloat64(float64(value)/1000, 0, 1)
}

func clampCandidate(candidate Candidate) Candidate {
	candidate.AdmissionIntercept = clampFloat64(candidate.AdmissionIntercept, minAdmissionIntercept, maxAdmissionIntercept)
	candidate.AdmissionSlope = clampFloat64(candidate.AdmissionSlope, minAdmissionSlope, maxAdmissionSlope)
	return candidate
}

func candidateKey(candidate Candidate) [2]int {
	candidate = clampCandidate(candidate)
	return [2]int{
		int(math.Round(candidate.AdmissionIntercept * 10000)),
		int(math.Round(candidate.AdmissionSlope * 10000)),
	}
}

func minCandidateDistance(samples []scoredCandidate, candidate Candidate) float64 {
	best := math.Inf(1)
	for _, sample := range samples {
		distance := candidateDistanceSquared(sample.Candidate, candidate)
		if distance < best {
			best = distance
		}
	}
	return best
}

func candidateDistanceSquared(a, b Candidate) float64 {
	di := (a.AdmissionIntercept - b.AdmissionIntercept) / 0.035
	ds := (a.AdmissionSlope - b.AdmissionSlope) / 0.075
	return di*di + ds*ds
}

func huber(value, delta float64) float64 {
	value = math.Abs(value)
	if value <= delta {
		return 0.5 * value * value
	}
	return delta * (value - 0.5*delta)
}

func improvementPct(baseline, candidate float64) float64 {
	if baseline <= 0 {
		if candidate < baseline {
			return 100
		}
		return 0
	}
	return 100 * (baseline - candidate) / baseline
}

func deltaPct(baseline, candidate float64) float64 {
	if baseline <= 0 {
		if candidate <= 0 {
			return 0
		}
		return 100
	}
	return 100 * (candidate - baseline) / baseline
}

func clampFloat64(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func positiveDelta(cur, prev int64) int64 {
	if cur <= prev {
		return 0
	}
	return cur - prev
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
