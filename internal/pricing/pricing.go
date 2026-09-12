// Package pricing applies user-supplied model rates to normalized usage.
package pricing

import "github.com/gregasher/bedrock-local-proxy/internal/config"

const (
	openAICacheReadMultiplier  = 0.10
	openAICacheWriteMultiplier = 1.25
)

// documentedOpenAICacheTargets is deliberately exact. AWS publishes the same
// 30-minute cache rates for these Bedrock Runtime inference IDs: reads cost
// 10% of normal input and writes cost 125% of normal input. Do not broaden
// this list with substring matching; an unknown target still requires explicit
// cache prices.
var documentedOpenAICacheTargets = map[string]struct{}{
	"us.openai.gpt-5.6-luna":      {},
	"global.openai.gpt-5.6-luna":  {},
	"in.openai.gpt-5.6-luna":      {},
	"us.openai.gpt-5.6-terra":     {},
	"global.openai.gpt-5.6-terra": {},
	"in.openai.gpt-5.6-terra":     {},
	"us.openai.gpt-5.6-sol":       {},
	"global.openai.gpt-5.6-sol":   {},
	"us.openai.gpt-6-astra":       {},
	"global.openai.gpt-6-astra":   {},
}

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
	cacheReadPrice, cacheWritePrice := cachePrices(model)
	if cacheRead > 0 && cacheReadPrice == nil {
		return 0, MissingCacheReadPrice
	}
	if cacheWrite > 0 && cacheWritePrice == nil {
		return 0, MissingCacheWritePrice
	}
	cost := float64(input)/1e6*(*model.InputPerMillion) + float64(*usage.OutputTokens)/1e6*(*model.OutputPerMillion)
	if cacheRead > 0 {
		cost += float64(cacheRead) / 1e6 * (*cacheReadPrice)
	}
	if cacheWrite > 0 {
		cost += float64(cacheWrite) / 1e6 * (*cacheWritePrice)
	}
	return cost, ""
}

func cachePrices(model config.ModelConfig) (*float64, *float64) {
	read, write := model.CacheReadInputPerMillion, model.CacheWriteInputPerMillion
	if model.InputPerMillion == nil {
		return read, write
	}
	if _, ok := documentedOpenAICacheTargets[model.BedrockModelID]; !ok {
		return read, write
	}
	if read == nil {
		value := *model.InputPerMillion * openAICacheReadMultiplier
		read = &value
	}
	if write == nil {
		value := *model.InputPerMillion * openAICacheWriteMultiplier
		write = &value
	}
	return read, write
}

func tokenCount(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
