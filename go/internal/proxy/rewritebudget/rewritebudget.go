package rewritebudget

import (
	"os"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxReasonableMemoryBytes int64 = 1 << 60
	maxInt64                 int64 = int64(^uint64(0) >> 1)
)

var (
	goMemorySamples = []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	detectedStaticMemoryBudgetBytes = sync.OnceValue(detectStaticMemoryBudgetBytes)
	detectedCgroupEventBase         = sync.OnceValue(readCurrentCgroupEvents)
	activeRewriteCount              atomic.Int64
)

type MemoryStatus struct {
	BudgetSource        string
	MemoryBudgetBytes   int64
	GoMemoryUsedBytes   int64
	CgroupCurrentBytes  int64
	EffectiveUsedBytes  int64
	CgroupHighEvents    int64
	CgroupMaxEvents     int64
	CgroupOOMEvents     int64
	CgroupOOMKillEvents int64
}

type cgroupMemoryEvents struct {
	High    int64
	Max     int64
	OOM     int64
	OOMKill int64
}

func Begin() func() {
	activeRewriteCount.Add(1)
	return func() {
		activeRewriteCount.Add(-1)
	}
}

func CurrentActiveCount() int64 {
	n := activeRewriteCount.Load()
	if n < 0 {
		return 0
	}
	return n
}

func CurrentMemoryStatus() MemoryStatus {
	memoryBudgetBytes, budgetSource := currentMemoryBudgetStatus()
	goUsed := currentGoMemoryUsedBytes()
	cgroupCurrent := readCgroupCurrentBytes()
	effectiveUsed := goUsed
	if cgroupCurrent > effectiveUsed {
		effectiveUsed = cgroupCurrent
	}

	curEvents := readCurrentCgroupEvents()
	baseEvents := detectedCgroupEventBase()

	return MemoryStatus{
		BudgetSource:        budgetSource,
		MemoryBudgetBytes:   memoryBudgetBytes,
		GoMemoryUsedBytes:   goUsed,
		CgroupCurrentBytes:  cgroupCurrent,
		EffectiveUsedBytes:  effectiveUsed,
		CgroupHighEvents:    positiveDelta(curEvents.High, baseEvents.High),
		CgroupMaxEvents:     positiveDelta(curEvents.Max, baseEvents.Max),
		CgroupOOMEvents:     positiveDelta(curEvents.OOM, baseEvents.OOM),
		CgroupOOMKillEvents: positiveDelta(curEvents.OOMKill, baseEvents.OOMKill),
	}
}

func currentMemoryBudgetStatus() (int64, string) {
	if limit := debug.SetMemoryLimit(-1); isUsableMemoryBudget(limit) {
		return limit, "gomemlimit"
	}
	if limit := detectedStaticMemoryBudgetBytes(); isUsableMemoryBudget(limit) {
		return limit, "cgroup"
	}
	if available := readMemAvailableBytes("/proc/meminfo"); isUsableMemoryBudget(available) {
		return available, "memavailable"
	}
	return 0, "default"
}

func detectStaticMemoryBudgetBytes() int64 {
	for _, path := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		if limit := readInt64File(path); isUsableMemoryBudget(limit) {
			return limit
		}
	}
	return 0
}

func currentGoMemoryUsedBytes() int64 {
	samples := make([]metrics.Sample, len(goMemorySamples))
	copy(samples, goMemorySamples)
	metrics.Read(samples)
	if len(samples) != 2 {
		return 0
	}
	if samples[0].Value.Kind() != metrics.KindUint64 || samples[1].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	total := samples[0].Value.Uint64()
	released := samples[1].Value.Uint64()
	if total < released || total > uint64(maxInt64) {
		return 0
	}
	return int64(total - released)
}

func readCgroupCurrentBytes() int64 {
	for _, path := range []string{
		"/sys/fs/cgroup/memory.current",
		"/sys/fs/cgroup/memory/memory.usage_in_bytes",
	} {
		if v := readInt64File(path); v > 0 {
			return v
		}
	}
	return 0
}

func readCurrentCgroupEvents() cgroupMemoryEvents {
	if ev, ok := readCgroupEventsV2("/sys/fs/cgroup/memory.events"); ok {
		return ev
	}
	if failcnt := readInt64File("/sys/fs/cgroup/memory/memory.failcnt"); failcnt > 0 {
		return cgroupMemoryEvents{Max: failcnt}
	}
	return cgroupMemoryEvents{}
}

func readCgroupEventsV2(path string) (cgroupMemoryEvents, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return cgroupMemoryEvents{}, false
	}
	events := cgroupMemoryEvents{}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(fields[0]) {
		case "high":
			events.High = value
			found = true
		case "max":
			events.Max = value
			found = true
		case "oom":
			events.OOM = value
			found = true
		case "oom_kill":
			events.OOMKill = value
			found = true
		}
	}
	return events, found
}

func readInt64File(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || strings.EqualFold(value, "max") {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func readMemAvailableBytes(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		if value > maxInt64/1024 {
			return 0
		}
		return value * 1024
	}
	return 0
}

func isUsableMemoryBudget(value int64) bool {
	return value > 0 && value < maxInt64 && value < maxReasonableMemoryBytes
}

func positiveDelta(cur, base int64) int64 {
	if cur <= base {
		return 0
	}
	return cur - base
}
