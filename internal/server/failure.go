package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

// Failure classes are the stable, metadata-only cause a supervisor reads to
// decide between retrying and failing. They come from upstream error
// identifiers, HTTP statuses, and transport classes, never from message text.
// A class absent from this map (upstream_error) has no known retry decision.
var failureRetryable = map[string]bool{
	"throttled":                 true,
	"service_unavailable":       true,
	"upstream_server":           true,
	"upstream_timeout":          true,
	"connectivity":              true,
	"stream_interrupted":        true,
	"shutdown":                  true,
	"credentials":               false,
	"quota_exhausted":           false,
	"reasoning_replay_rejected": false,
	"invalid_request":           false,
	"model_unavailable":         false,
	"budget_exhausted":          false,
	"canceled":                  false,
	"local_validation":          false,
	"proxy_configuration":       false,
}

// failureIdentities maps lowercased Bedrock exception names and OpenAI or
// Anthropic error codes and types to a class.
var failureIdentities = map[string]string{
	"throttlingexception":           "throttled",
	"too_many_requests":             "throttled",
	"serviceunavailableexception":   "service_unavailable",
	"modelnotreadyexception":        "service_unavailable",
	"server_is_overloaded":          "service_unavailable",
	"slow_down":                     "service_unavailable",
	"internalserverexception":       "upstream_server",
	"server_error":                  "upstream_server",
	"internal_error":                "upstream_server",
	"api_error":                     "upstream_server",
	"modeltimeoutexception":         "upstream_timeout",
	"modelstreamerrorexception":     "stream_interrupted",
	"accessdeniedexception":         "credentials",
	"unrecognizedclientexception":   "credentials",
	"expiredtokenexception":         "credentials",
	"authentication_error":          "credentials",
	"permission_error":              "credentials",
	"servicequotaexceededexception": "quota_exhausted",
	"insufficient_quota":            "quota_exhausted",
	"validationexception":           "invalid_request",
	"invalid_request_error":         "invalid_request",
	"validation_error":              "invalid_request",
	"context_length_exceeded":       "invalid_request",
	"resourcenotfoundexception":     "model_unavailable",
	"model_not_found":               "model_unavailable",
}

var upstreamCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func identityClass(id string) string {
	id = strings.ToLower(id)
	if class, ok := failureIdentities[id]; ok {
		return class
	}
	switch {
	case strings.HasPrefix(id, "rate_limit"):
		return "throttled"
	case strings.HasPrefix(id, "overloaded"):
		return "service_unavailable"
	}
	return ""
}

func statusClass(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "throttled"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "upstream_server"
	case http.StatusRequestTimeout:
		return "upstream_timeout"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "credentials"
	case http.StatusPaymentRequired:
		return "quota_exhausted"
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return "invalid_request"
	case http.StatusNotFound:
		return "model_unavailable"
	}
	return "upstream_error"
}

func transportFailureClass(err error) string {
	switch transport.ClassOf(err) {
	case transport.FailureCanceled:
		return "canceled"
	case transport.FailureCredentialsExpired, transport.FailureCredentialsUnavailable:
		return "credentials"
	case transport.FailureTimeout:
		return "upstream_timeout"
	case transport.FailureConnectivity:
		return "connectivity"
	case transport.FailureSigning, transport.FailureConfiguration, transport.FailureRedirect:
		return "proxy_configuration"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream_timeout"
	}
	return "upstream_error"
}

// sanitizedCode returns id only when it is a short identifier, so an error
// message placed in a code field can never reach the record.
func sanitizedCode(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.LastIndexByte(id, '#'); i >= 0 {
		id = id[i+1:]
	}
	if !upstreamCodePattern.MatchString(id) || strings.EqualFold(id, "error") {
		return ""
	}
	return id
}

// upstreamIdentities lists the error identifiers of a failed response, most
// explicit first: the Bedrock errortype header, then the JSON body.
func upstreamIdentities(header http.Header, body []byte) []string {
	var ids []string
	if value := header.Get("X-Amzn-Errortype"); value != "" {
		value, _, _ = strings.Cut(value, ":")
		ids = append(ids, value)
	}
	return append(ids, bodyIdentities(body)...)
}

// bodyIdentities reads error.code, error.type, __type, code, and type from one
// JSON error body or stream event.
func bodyIdentities(body []byte) []string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil
	}
	var ids []string
	var nested map[string]json.RawMessage
	if json.Unmarshal(fields["error"], &nested) == nil {
		ids = append(ids, jsonString(nested["code"]), jsonString(nested["type"]))
	}
	return append(ids, jsonString(fields["__type"]), jsonString(fields["code"]), jsonString(fields["type"]))
}

func jsonString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// classifyFailure picks the class of the first recognized identifier and falls
// back to the status. It also returns the identifier worth recording.
func classifyFailure(status int, ids []string) (string, string) {
	code := ""
	for _, id := range ids {
		id = sanitizedCode(id)
		if id == "" {
			continue
		}
		if class := identityClass(id); class != "" {
			return class, id
		}
		if code == "" {
			code = id
		}
	}
	return statusClass(status), code
}

// classifyUpstreamResponse records the cause of a non-2xx upstream response.
// A 400 validation rejection of replayed encrypted reasoning is separated
// because retrying the same input cannot succeed.
func classifyUpstreamResponse(result *CompletionResult, status int, header http.Header, body []byte, encryptedReasoning bool) {
	ids := upstreamIdentities(header, body)
	result.FailureClass, result.UpstreamCode = classifyFailure(status, ids)
	if encryptedReasoning && status == http.StatusBadRequest {
		for _, id := range ids {
			if strings.EqualFold(id, "validation_error") || strings.EqualFold(id, "invalid_request_error") {
				result.FailureClass = "reasoning_replay_rejected"
				break
			}
		}
	}
	result.RetryAfterMS = retryAfterMS(header)
}

// retryAfterMS accepts only integer seconds; HTTP dates are ignored.
func retryAfterMS(header http.Header) *int64 {
	seconds, err := strconv.ParseInt(strings.TrimSpace(header.Get("Retry-After")), 10, 64)
	if err != nil || seconds < 0 || seconds > math.MaxInt64/1000 {
		return nil
	}
	ms := seconds * 1000
	return &ms
}

func validClientID(value string) string {
	if clientIDPattern.MatchString(value) {
		return value
	}
	return ""
}
