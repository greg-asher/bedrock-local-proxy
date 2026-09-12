// Package pricing applies user-supplied model rates to normalized usage.
package pricing

import "github.com/gregasher/bedrock-local-proxy/internal/config"

// Reason explains why a usage record cannot be priced.
type Reason string

const (
	MissingUsage           Reason = "missing input or output token usage"
	UncoveredBilling       Reason = "response contains an unsupported nonzero billing dimension"
	MissingInputPrice      Reason = "input_per_million is missing"
	MissingOutputPrice     Reason = "output_per_million is missing"
	MissingCacheReadPrice  Reason = "cache_read_input_per_million is missing for nonzero cache reads"
	MissingCacheWritePrice Reason = "cache_write_input_per_million is missing for nonzero cache writes"
	InvalidCacheBreakdown  Reason = "cache token counts exceed the reported input total"
	InvalidTokenCount      Reason = "token usage contains a negative count"
)

// Usage contains only the token dimensions needed for a cost estimate.
type Usage struct {
	InputTokens                    *int64
	OutputTokens                   *int64
	CacheReadInputTokens           *int64
	CacheWriteInputTokens          *int64
	InputTokensIncludeCache        bool
	ObservedUncoveredBillingFields bool
}

// Estimate returns the estimated cost and an empty reason, or a stable reason
// explaining why the record cannot be priced.
func Estimate(model config.ModelConfig, usage Usage) (float64, Reason) {
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		return 0, MissingUsage
	}
	if *usage.InputTokens < 0 || *usage.OutputTokens < 0 || tokenCount(usage.CacheReadInputTokens) < 0 || tokenCount(usage.CacheWriteInputTokens) < 0 {
		return 0, InvalidTokenCount
	}
	if usage.ObservedUncoveredBillingFields {
		return 0, UncoveredBilling
	}
	if model.InputPerMillion == nil {
		return 0, MissingInputPrice
	}
	if model.OutputPerMillion == nil {
		return 0, MissingOutputPrice
	}
	cacheRead := tokenCount(usage.CacheReadInputTokens)
	cacheWrite := tokenCount(usage.CacheWriteInputTokens)
	input := *usage.InputTokens
	if usage.InputTokensIncludeCache {
		input -= cacheRead + cacheWrite
		if input < 0 {
			return 0, InvalidCacheBreakdown
		}
	}
	if cacheRead > 0 && model.CacheReadInputPerMillion == nil {
		return 0, MissingCacheReadPrice
	}
	if cacheWrite > 0 && model.CacheWriteInputPerMillion == nil {
		return 0, MissingCacheWritePrice
	}
	cost := float64(input)/1e6*(*model.InputPerMillion) + float64(*usage.OutputTokens)/1e6*(*model.OutputPerMillion)
	if cacheRead > 0 {
		cost += float64(cacheRead) / 1e6 * (*model.CacheReadInputPerMillion)
	}
	if cacheWrite > 0 {
		cost += float64(cacheWrite) / 1e6 * (*model.CacheWriteInputPerMillion)
	}
	return cost, ""
}

func tokenCount(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
