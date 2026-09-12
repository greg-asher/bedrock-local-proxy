package pricing

import (
	"math"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

func TestEstimatePricesOpenAIAndMessagesCacheSemantics(t *testing.T) {
	inputPrice, outputPrice := 2.0, 10.0
	cacheReadPrice, cacheWritePrice := 0.2, 2.5
	model := config.ModelConfig{
		InputPerMillion:           &inputPrice,
		OutputPerMillion:          &outputPrice,
		CacheReadInputPerMillion:  &cacheReadPrice,
		CacheWriteInputPerMillion: &cacheWritePrice,
	}
	input, output := int64(1000), int64(100)
	cacheRead, cacheWrite := int64(400), int64(100)
	for _, test := range []struct {
		name          string
		includesCache bool
		want          float64
	}{
		{name: "OpenAI input includes cache subsets", includesCache: true, want: 0.00233},
		{name: "Messages input excludes cache dimensions", includesCache: false, want: 0.00333},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason := Estimate(model, Usage{InputTokens: &input, OutputTokens: &output, CacheReadInputTokens: &cacheRead, CacheWriteInputTokens: &cacheWrite, InputTokensIncludeCache: test.includesCache})
			if reason != "" || math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("Estimate()=(%.8f,%q), want (%.8f, empty)", got, reason, test.want)
			}
		})
	}
}

func TestEstimateExplainsUnavailableCosts(t *testing.T) {
	inputPrice, outputPrice := 2.0, 10.0
	input, output, cacheRead, negative := int64(10), int64(2), int64(4), int64(-1)
	for _, test := range []struct {
		name  string
		model config.ModelConfig
		usage Usage
		want  Reason
	}{
		{name: "usage", model: config.ModelConfig{InputPerMillion: &inputPrice, OutputPerMillion: &outputPrice}, want: MissingUsage},
		{name: "uncovered", model: config.ModelConfig{InputPerMillion: &inputPrice, OutputPerMillion: &outputPrice}, usage: Usage{InputTokens: &input, OutputTokens: &output, ObservedUncoveredBillingFields: true}, want: UncoveredBilling},
		{name: "input price", model: config.ModelConfig{OutputPerMillion: &outputPrice}, usage: Usage{InputTokens: &input, OutputTokens: &output}, want: MissingInputPrice},
		{name: "output price", model: config.ModelConfig{InputPerMillion: &inputPrice}, usage: Usage{InputTokens: &input, OutputTokens: &output}, want: MissingOutputPrice},
		{name: "cache read price", model: config.ModelConfig{InputPerMillion: &inputPrice, OutputPerMillion: &outputPrice}, usage: Usage{InputTokens: &input, OutputTokens: &output, CacheReadInputTokens: &cacheRead}, want: MissingCacheReadPrice},
		{name: "invalid cache", model: config.ModelConfig{InputPerMillion: &inputPrice, OutputPerMillion: &outputPrice, CacheReadInputPerMillion: &inputPrice}, usage: Usage{InputTokens: &cacheRead, OutputTokens: &output, CacheReadInputTokens: &input, InputTokensIncludeCache: true}, want: InvalidCacheBreakdown},
		{name: "negative token", model: config.ModelConfig{InputPerMillion: &inputPrice, OutputPerMillion: &outputPrice}, usage: Usage{InputTokens: &negative, OutputTokens: &output}, want: InvalidTokenCount},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, got := Estimate(test.model, test.usage); got != test.want {
				t.Fatalf("Estimate() reason=%q want %q", got, test.want)
			}
		})
	}
}
