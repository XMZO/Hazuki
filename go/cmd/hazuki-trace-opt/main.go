package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"hazuki-go/internal/proxy/rewritebudget"
	"hazuki-go/internal/tools/rewriteopt"
)

type tracePayload struct {
	Model rewritebudget.RuntimeModelStatus   `json:"model"`
	Trace []rewritebudget.RuntimeTraceRecord `json:"trace"`
}

type jsonOutput struct {
	InputSamples int               `json:"inputSamples"`
	Result       rewriteopt.Result `json:"result"`
}

func main() {
	var (
		inputPath  string
		iterations int
		jsonMode   bool
	)

	flag.StringVar(&inputPath, "in", "-", "trace json file path, or - for stdin")
	flag.IntVar(&iterations, "iterations", 96, "optimizer iterations")
	flag.BoolVar(&jsonMode, "json", false, "print machine-readable json")
	flag.Parse()

	records, baseline, err := loadTrace(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load trace: %v\n", err)
		os.Exit(1)
	}

	result := rewriteopt.Optimize(records, baseline, iterations)
	if jsonMode {
		payload := jsonOutput{
			InputSamples: len(records),
			Result:       result,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}

	fmt.Printf("Trace samples: %d\n", len(records))
	fmt.Printf("Observation samples: %d\n", result.Observations)
	fmt.Printf("Explored candidates: %d\n", result.ExploredCandidates)
	fmt.Printf("Algorithm: %s / %s\n", result.Algorithm, result.Acquisition)
	fmt.Printf("Split: %s\n", result.SplitStrategy)
	fmt.Printf("Context model: %s\n", result.ContextModel)
	printSummary("Train", result.Train)
	printSummary("Validation", result.Validation)
	fmt.Printf("Gate: %s (%s)\n", gateLabel(result.Gate.Recommended), result.Gate.Reason)
	fmt.Printf("Gate improvement: train %.2f%% / validation %.2f%%\n", result.Gate.TrainImprovementPct, result.Gate.ValidationImprovementPct)
	if result.Gate.WorstContextKey != "" {
		fmt.Printf("Worst validation context: %s (%.2f%%)\n", result.Gate.WorstContextKey, result.Gate.WorstContextImprovementPct)
	}
	fmt.Println()
	if result.Gate.Recommended {
		fmt.Println("Recommended env:")
		fmt.Printf("HAZUKI_REWRITE_ADMISSION_INTERCEPT=%.4f\n", result.Best.AdmissionIntercept)
		fmt.Printf("HAZUKI_REWRITE_ADMISSION_SLOPE=%.4f\n", result.Best.AdmissionSlope)
		return
	}

	fmt.Println("Keep current env. Challenger env for manual replay only:")
	fmt.Printf("HAZUKI_REWRITE_ADMISSION_INTERCEPT=%.4f\n", result.Best.AdmissionIntercept)
	fmt.Printf("HAZUKI_REWRITE_ADMISSION_SLOPE=%.4f\n", result.Best.AdmissionSlope)
}

func loadTrace(path string) ([]rewritebudget.RuntimeTraceRecord, rewriteopt.Candidate, error) {
	data, err := readAll(path)
	if err != nil {
		return nil, rewriteopt.DefaultCandidate(), err
	}

	payload := tracePayload{}
	if err := json.Unmarshal(data, &payload); err == nil && len(payload.Trace) > 0 {
		return payload.Trace, baselineFromModel(payload.Model), nil
	}

	var trace []rewritebudget.RuntimeTraceRecord
	if err := json.Unmarshal(data, &trace); err != nil {
		return nil, rewriteopt.DefaultCandidate(), err
	}
	return trace, rewriteopt.DefaultCandidate(), nil
}

func baselineFromModel(model rewritebudget.RuntimeModelStatus) rewriteopt.Candidate {
	baseline := rewriteopt.DefaultCandidate()
	if model.AdmissionSamples > 0 {
		baseline.AdmissionIntercept = model.AdmissionIntercept
		baseline.AdmissionSlope = model.AdmissionSlope
	}
	return baseline
}

func readAll(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func printSummary(name string, summary rewriteopt.DatasetSummary) {
	fmt.Printf("%s: %d obs / %d contexts / %.2f%% improvement\n", name, summary.Observations, summary.Contexts, summary.ImprovementPct)
	fmt.Printf("  baseline %.6f (risk %.6f, cpu %.6f, jitter %.6f)\n", summary.Baseline.Total, summary.Baseline.Risk, summary.Baseline.CPU, summary.Baseline.Jitter)
	fmt.Printf("  candidate %.6f (risk %.6f, cpu %.6f, jitter %.6f)\n", summary.Candidate.Total, summary.Candidate.Risk, summary.Candidate.CPU, summary.Candidate.Jitter)
}

func gateLabel(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
