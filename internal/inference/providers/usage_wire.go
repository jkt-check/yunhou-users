package providers

import (
	"math"

	"github.com/yunhou/users/internal/inference/domain"
)

// addUsageDetails requires a known main counter; optional details cannot
// establish it. Invalid arithmetic leaves the derived counter unknown.
func addUsageDetails(main *int64, details ...*int64) *int64 {
	if main == nil || *main < 0 {
		return nil
	}
	sum := *main
	for _, detail := range details {
		if detail == nil {
			continue
		}
		if *detail < 0 || *detail > math.MaxInt64-sum {
			return nil
		}
		sum += *detail
	}
	return &sum
}

func derivedUsageTotal(input, output *int64) *int64 {
	if output == nil {
		return nil
	}
	return addUsageDetails(input, output)
}

func openAIUsageShape(b domain.UsageBuckets, reportedTotal *int64) map[string]any {
	usage := map[string]any{}
	if b.InputTokens != nil {
		usage["prompt_tokens"] = *b.InputTokens
	}
	if b.OutputTokens != nil {
		usage["completion_tokens"] = *b.OutputTokens
	}
	total := reportedTotal
	if total == nil {
		total = derivedUsageTotal(b.InputTokens, b.OutputTokens)
	}
	if total != nil {
		usage["total_tokens"] = *total
	}
	if b.CacheReadTokens != nil {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *b.CacheReadTokens}
	}
	if b.CacheWriteTokens != nil {
		usage["cache_creation_input_tokens"] = *b.CacheWriteTokens
	}
	if b.ReasoningTokens != nil {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": *b.ReasoningTokens}
	}
	return usage
}
