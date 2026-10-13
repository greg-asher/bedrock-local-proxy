package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/greg-asher/bedrock-local-proxy/internal/pricing"
)

// BudgetSpend is the accounting view a budget is enforced against. Estimated
// is this run's accumulated cost in the currency of the configured model
// prices. Uncounted is the number of successful requests whose cost could not
// be estimated for a configuration reason, which means real spend has already
// escaped the total.
type BudgetSpend struct {
	Estimated float64
	Uncounted int
}

// budgetLimit refuses generation requests once this run can no longer be held
// to its ceiling, either because the ceiling is reached or because spend has
// gone uncounted.
type budgetLimit struct {
	limit float64
	spend func() BudgetSpend
}

// SetBudget installs a spend ceiling for one run, expressed in the currency of
// the configured model prices. Once spend reaches it, generation requests are
// refused without being proxied upstream. It must be configured before serving
// requests.
func (s *Server) SetBudget(limit float64, spend func() BudgetSpend) {
	if spend == nil {
		s.budget = nil
		return
	}
	s.budget = &budgetLimit{limit: limit, spend: spend}
}

func generationEndpoint(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/responses" || path == "/v1/messages"
}

// refuseExhaustedBudget rejects a generation request when this run's ceiling
// can no longer be honored. It reports whether the request was refused.
func (s *Server) refuseExhaustedBudget(w http.ResponseWriter, r *http.Request, started time.Time) bool {
	if s.budget == nil {
		return false
	}
	spend := s.budget.spend()
	switch {
	case spend.Uncounted > 0:
		// Spend has already escaped the total, so the ceiling is unenforceable
		// from here on. Failing closed is the only honest option for a budget.
		s.refuseBudget(w, r, started, "", fmt.Sprintf("%d completed request(s) could not be priced, so spend can no longer be counted against the session budget of %s; add the missing model prices and restart the proxy", spend.Uncounted, formatBudget(s.budget.limit)))
	case spend.Estimated >= s.budget.limit:
		s.refuseBudget(w, r, started, "", fmt.Sprintf("session budget of %s exhausted (estimated spend %s); restart the proxy with a higher --budget", formatBudget(s.budget.limit), formatBudget(spend.Estimated)))
	default:
		return false
	}
	return true
}

// refuseUnpricedModel rejects a request whose alias carries no prices, because
// its cost could never be counted against the ceiling. The alias is known only
// after the request is transformed, so each generation handler calls this once
// it has resolved one. It reports whether the request was refused.
func (s *Server) refuseUnpricedModel(w http.ResponseWriter, r *http.Request, started time.Time, localModel string) bool {
	if s.budget == nil {
		return false
	}
	model, ok := s.cfg.Models[localModel]
	if !ok {
		return false
	}
	reason := pricing.Configured(model)
	if reason == "" {
		return false
	}
	s.refuseBudget(w, r, started, localModel, fmt.Sprintf("model %q cannot be priced (%s), so its cost cannot be counted against the session budget; add the missing prices to this model or restart the proxy without --budget", localModel, reason))
	return true
}

// refuseBudget answers in the error shape the calling client expects. 402 is
// used rather than 429 because the refusal is terminal for the run: retrying
// cannot succeed until the proxy is restarted. localModel is empty when the
// ceiling is checked before the request body has been transformed.
func (s *Server) refuseBudget(w http.ResponseWriter, r *http.Request, started time.Time, localModel, message string) {
	status := http.StatusPaymentRequired
	if r.URL.Path == "/v1/messages" {
		s.writeAnthropicError(w, status, message, "permission_error")
	} else {
		s.writeOpenAIErrorDetails(w, status, message, "insufficient_quota", nil, "budget_exhausted")
	}
	s.finishCompletion(r, started, CompletionResult{Endpoint: r.URL.Path, LocalModel: localModel, HTTPStatus: &status, Outcome: CompletionFailed, FailureCategory: "budget_exhausted", FailureClass: "budget_exhausted"})
}

func formatBudget(value float64) string { return fmt.Sprintf("$%.6f", value) }
