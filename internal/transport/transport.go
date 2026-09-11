// Package transport sends Bedrock Runtime requests with AWS SigV4
// authentication. It owns the credential and HTTP lifecycle shared by all
// protocol handlers; it does not know about any client protocol or payload
// schema.
package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

const (
	// bedrockSigningService is the SigV4 service name used by the Bedrock
	// Runtime API. Its hostname contains "bedrock-runtime", but AWS signs this
	// API with the "bedrock" service name.
	bedrockSigningService = "bedrock"
	defaultDialTimeout    = 10 * time.Second
	defaultHeaderTimeout  = 30 * time.Second
)

var regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// FailureClass is a safe category handlers can use to choose a client-facing
// status. Error messages intentionally do not include request headers, body,
// credentials, or upstream response content.
type FailureClass string

const (
	FailureCredentialsUnavailable FailureClass = "credentials_unavailable"
	FailureCredentialsExpired     FailureClass = "credentials_expired"
	FailureSigning                FailureClass = "signing"
	FailureConnectivity           FailureClass = "connectivity"
	FailureTimeout                FailureClass = "timeout"
	FailureCanceled               FailureClass = "canceled"
	FailureRedirect               FailureClass = "redirect_blocked"
	FailureConfiguration          FailureClass = "configuration"
)

// Failure is returned for transport failures. Class is stable for handlers;
// the wrapped cause is retained only for errors.Is/errors.As by internal code.
// Error intentionally contains only a generic, safe description.
type Failure struct {
	Class FailureClass
	cause error
}

func (f *Failure) Error() string {
	if f == nil {
		return "<nil>"
	}
	switch f.Class {
	case FailureCredentialsExpired:
		return "AWS credentials have expired; sign in again for the configured profile"
	case FailureCredentialsUnavailable:
		return "AWS credentials are unavailable for the configured profile"
	case FailureSigning:
		return "AWS request signing failed"
	case FailureConnectivity:
		return "could not reach the AWS upstream"
	case FailureTimeout:
		return "AWS upstream connection timed out"
	case FailureCanceled:
		return "AWS request was canceled"
	case FailureRedirect:
		return "AWS upstream redirect was blocked"
	case FailureConfiguration:
		return "AWS transport configuration is invalid"
	default:
		return "AWS transport failed"
	}
}

func (f *Failure) Unwrap() error { return f.cause }

// ClassOf returns the transport category for err. Non-transport errors return
// an empty class.
func ClassOf(err error) FailureClass {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Class
	}
	return ""
}

// Transport signs and executes requests against one fixed regional Bedrock
// Runtime authority. The endpoint and HTTP client are deliberately not part
// of user configuration; the unexported constructor below is used by local
// contract tests to substitute a fake provider and local server.
type Transport struct {
	region  string
	profile string
	baseURL *url.URL
	client  *http.Client
	creds   aws.CredentialsProvider
	signer  *v4.Signer
	now     func() time.Time

	// Keep the credentials cache at the transport boundary. The SDK cache
	// owns expiry and refresh; no raw keys are stored by this package.
	cacheOnce sync.Once
}

// New loads the official AWS SDK provider chain for profile and region. It
// does not retrieve credentials until the first request is signed, so local
// startup does not require an active AWS session.
func New(ctx context.Context, profile, region string) (*Transport, error) {
	if err := validateRegion(region); err != nil {
		return nil, err
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithSharedConfigProfile(profile),
	)
	if err != nil {
		return nil, newFailure(FailureCredentialsUnavailable, err)
	}
	return newWithProvider(profile, region, regionalEndpoint(region), cfg.Credentials, nil, nil)
}

// newWithProvider is intentionally package-private. It gives tests a real AWS
// SDK credential-provider shape and a local HTTP authority without exposing a
// remote endpoint override in the product configuration.
func newWithProvider(profile, region, endpoint string, provider aws.CredentialsProvider, client *http.Client, signer *v4.Signer) (*Transport, error) {
	if err := validateRegion(region); err != nil {
		return nil, err
	}
	base, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, newFailure(FailureConfiguration, errors.New("credentials provider is nil"))
	}
	if client == nil {
		client = defaultHTTPClient()
	} else {
		client = cloneHTTPClient(client)
	}
	if signer == nil {
		signer = v4.NewSigner()
	}
	return &Transport{
		region:  region,
		profile: profile,
		baseURL: base,
		client:  client,
		creds:   aws.NewCredentialsCache(provider),
		signer:  signer,
		now:     time.Now,
	}, nil
}

func regionalEndpoint(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

func validateRegion(region string) error {
	if !regionPattern.MatchString(strings.TrimSpace(region)) {
		return newFailure(FailureConfiguration, errors.New("invalid AWS region"))
	}
	return nil
}

func parseEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, newFailure(FailureConfiguration, errors.New("invalid AWS endpoint"))
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, newFailure(FailureConfiguration, errors.New("invalid AWS endpoint scheme"))
	}
	parsed.Fragment = ""
	return parsed, nil
}

// Do signs and executes req against the configured regional authority. The
// incoming scheme, host, and user information are ignored. The path and
// query are retained because protocol handlers choose the Bedrock operation.
// There is no automatic retry and no whole-response deadline, so an active
// streaming response may remain open after its bounded connection/header
// waits have passed.
func (t *Transport) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if t == nil || t.baseURL == nil || t.client == nil || t.creds == nil || t.signer == nil {
		return nil, newFailure(FailureConfiguration, errors.New("transport is not initialized"))
	}
	if req == nil {
		return nil, newFailure(FailureConfiguration, errors.New("request is nil"))
	}
	if ctx == nil {
		ctx = context.Background()
	}

	out, body, err := prepareRequest(ctx, req, t.baseURL)
	if err != nil {
		return nil, classifyPreparationError(err)
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, newFailure(FailureCanceled, err)
		}
		return nil, newFailure(FailureTimeout, err)
	}

	// Retrieval occurs after all request transformations, immediately before
	// signing the final method, authority, headers, path, query, and body.
	credentials, err := t.creds.Retrieve(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, newFailure(FailureCanceled, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, newFailure(FailureTimeout, err)
		}
		return nil, newFailure(classifyCredentialError(err), err)
	}
	payloadHash := sha256.Sum256(body)
	if err := t.signer.SignHTTP(ctx, credentials, out, hex.EncodeToString(payloadHash[:]), bedrockSigningService, t.region, t.now()); err != nil {
		return nil, newFailure(FailureSigning, err)
	}

	response, err := t.client.Do(out)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, classifyHTTPError(err)
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, newFailure(FailureRedirect, errors.New("upstream returned redirect"))
	}
	stripHopByHopHeaders(response.Header)
	return response, nil
}

func prepareRequest(ctx context.Context, incoming *http.Request, endpoint *url.URL) (*http.Request, []byte, error) {
	var body []byte
	if incoming.Body != nil {
		var err error
		body, err = io.ReadAll(incoming.Body)
		if err != nil {
			return nil, nil, err
		}
	}

	destination := *endpoint
	destination.Path = incoming.URL.Path
	destination.RawPath = incoming.URL.RawPath
	destination.RawQuery = incoming.URL.RawQuery
	destination.Fragment = ""
	if destination.Path == "" {
		destination.Path = "/"
	}
	if !strings.HasPrefix(destination.Path, "/") {
		destination.Path = "/" + destination.Path
	}

	// Clone after reading the body so the outgoing request owns a fresh reader.
	out := incoming.Clone(ctx)
	out.URL = &destination
	out.RequestURI = ""
	out.Host = destination.Host
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = nil // prevents net/http from replaying a generation request
	out.ContentLength = int64(len(body))
	stripSensitiveHeaders(out.Header)
	stripHopByHopHeaders(out.Header)
	// Content-Length is represented by Request.ContentLength in net/http. A
	// stale header from the local client must never survive transformation.
	deleteHeaderFold(out.Header, "Content-Length")
	return out, body, nil
}

func stripSensitiveHeaders(header http.Header) {
	for key := range header {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "x-api-key" || lower == "x-bedrock-proxy-catalog" || strings.HasPrefix(lower, "x-amz-") {
			delete(header, key)
		}
	}
}

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func stripHopByHopHeaders(header http.Header) {
	for key, values := range header {
		if !strings.EqualFold(key, "Connection") {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				deleteHeaderFold(header, strings.TrimSpace(token))
			}
		}
	}
	for key := range header {
		if _, ok := hopByHopHeaders[strings.ToLower(key)]; ok {
			delete(header, key)
		}
	}
}

func deleteHeaderFold(header http.Header, name string) {
	for key := range header {
		if strings.EqualFold(key, name) {
			delete(header, key)
		}
	}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: defaultDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   defaultDialTimeout,
			ResponseHeaderTimeout: defaultHeaderTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
		// Timeout is intentionally zero. Connection and response-header waits
		// are bounded by the transport above; active model streams are not cut
		// off by a short whole-response deadline.
		Timeout: 0,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirect blocked")
		},
	}
}

func cloneHTTPClient(client *http.Client) *http.Client {
	clone := *client
	// Always replace a caller-supplied policy. A custom policy could follow a
	// redirect and forward the signed request to an authority outside the
	// configured Bedrock endpoint.
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("redirect blocked")
	}
	// A caller-supplied timeout could terminate an active stream; the shared
	// transport owns connection/header limits and must not impose one.
	clone.Timeout = 0
	return &clone
}

func classifyCredentialError(err error) FailureClass {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "expired") || strings.Contains(message, "invalid_grant") || strings.Contains(message, "sso") || strings.Contains(message, "token is expired") {
		return FailureCredentialsExpired
	}
	return FailureCredentialsUnavailable
}

func classifyPreparationError(err error) *Failure {
	if errors.Is(err, context.Canceled) {
		return newFailure(FailureCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newFailure(FailureTimeout, err)
	}
	return newFailure(FailureConnectivity, err)
}

func classifyHTTPError(err error) *Failure {
	if errors.Is(err, context.Canceled) {
		return newFailure(FailureCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newFailure(FailureTimeout, err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && strings.Contains(strings.ToLower(urlErr.Error()), "redirect blocked") {
		return newFailure(FailureRedirect, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return newFailure(FailureTimeout, err)
	}
	return newFailure(FailureConnectivity, err)
}

func newFailure(class FailureClass, cause error) *Failure {
	return &Failure{Class: class, cause: cause}
}

// Ensure the package's documented provider shape is checked against the SDK
// interface at compile time.
var _ aws.CredentialsProvider = (*aws.CredentialsCache)(nil)
