package billing

import (
	"encoding/json"
	"sort"
)

func parseSteps(raw string) []PriceStep {
	var steps []PriceStep
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &steps)
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].UpToBytes < steps[j].UpToBytes })
	return steps
}

func parseDiscounts(raw string) []DiscountWindow {
	var windows []DiscountWindow
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &windows)
	}
	return windows
}

// tieredCost prices the first totalBytes of usage using cumulative,
// left-closed/right-open tiers. The final tier's unit price applies above the
// last boundary. Tier boundaries and units are fixed here so B4 acceptance
// ("阶梯 + 优惠时段 + 封顶" matches manual calculation) is reproducible.
func tieredCost(totalBytes int64, steps []PriceStep) int64 {
	if totalBytes <= 0 || len(steps) == 0 {
		return 0
	}
	const gb = int64(1 << 30)
	var cost int64
	var lower int64
	for _, step := range steps {
		if step.UpToBytes <= lower && step.UpToBytes > 0 {
			continue
		}
		upper := totalBytes
		if step.UpToBytes > 0 && step.UpToBytes < upper {
			upper = step.UpToBytes
		}
		if upper > lower {
			cost += ((upper - lower) * step.PricePerGBFen) / gb
		}
		lower = step.UpToBytes
		if totalBytes <= step.UpToBytes {
			return cost
		}
	}
	// Beyond the last boundary, keep the last tier's unit price.
	if len(steps) > 0 && totalBytes > lower {
		last := steps[len(steps)-1].PricePerGBFen
		cost += ((totalBytes - lower) * last) / gb
	}
	return cost
}
