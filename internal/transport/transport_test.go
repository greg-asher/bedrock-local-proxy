package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type fakeProvider struct {
	mu     sync.Mutex
	values []aws.Credentials
	calls  int
}

func (p *fakeProvider) Retrieve(context.Context) (aws.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.values) == 0 {
		return aws.Credentials{}, errors.New("no fake credentials")
	}
	value := p.values[0]
	if len(p.values) > 1 {
		p.values = p.values[1:]
	}
	return value, nil
}

func (p *fakeProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testTransport(t *testing.T, endpoint string, provider aws.CredentialsProvider, client *http.Client) *Transport {
	t.Helper()
	transport, err := newWithProvider("test-profile", "us-east-2", endpoint, provider, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport.now = func() time.Time { return time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC) }
	return transport
}

func TestDoSignsBedrockRequestWithTemporarySessionCredentials(t *testing.T) {
	const payload = `{"inputText":"hello"}`
	var got *http.Request
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	provider := &fakeProvider{values: []aws.Credentials{{
		AccessKeyID:     "ASIAEXAMPLE",
		SecretAccessKey: "secret-example",
		SessionToken:    "session-example",
		CanExpire:       true,
		Expires:         time.Date(2025, time.January, 2, 4, 4, 5, 0, time.UTC),
	}}}
	transport := testTransport(t, server.URL, provider, server.Client())

	request, err := http.NewRequest(http.MethodPost, "https://client.example.invalid/model/target/invoke?trace=1", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.Do(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got == nil {
		t.Fatal("server did not receive a request")
	}
	if got.Host != strings.TrimPrefix(server.URL, "http://") {
		t.Fatalf("Host = %q, want %q", got.Host, strings.TrimPrefix(server.URL, "http://"))
	}
	if got.URL.Path != "/model/target/invoke" || got.URL.RawQuery != "trace=1" {
		t.Fatalf("upstream URL = %s, want configured path and query", got.URL)
	}
	if got.Header.Get("X-Amz-Security-Token") != "session-example" {
		t.Fatalf("session token = %q, want temporary credential token", got.Header.Get("X-Amz-Security-Token"))
	}
	authorization := got.Header.Get("Authorization")
	for _, want := range []string{
		"Credential=ASIAEXAMPLE/20250102/us-east-2/bedrock/aws4_request",
		"SignedHeaders=",
		"Signature=",
	} {
		if !strings.Contains(authorization, want) {
			t.Fatalf("Authorization = %q, missing %q", authorization, want)
		}
	}
	if got.Header.Get("X-Amz-Content-Sha256") != "" {
		wantHash := sha256.Sum256([]byte(payload))
		if got.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(wantHash[:]) {
			t.Fatalf("payload hash = %q, want %q", got.Header.Get("X-Amz-Content-Sha256"), hex.EncodeToString(wantHash[:]))
		}
	}
	if got.Header.Get("X-Amz-Date") != "20250102T030405Z" {
		t.Fatalf("x-amz-date = %q, want fixed signing time", got.Header.Get("X-Amz-Date"))
	}
	if string(gotBody) != payload {
		t.Fatalf("body = %q, want %q", gotBody, payload)
	}
	if got.ContentLength != int64(len(payload)) {
		t.Fatalf("ContentLength = %d, want %d", got.ContentLength, len(payload))
	}
}

func TestExpiringProviderSuppliesReplacementCredentials(t *testing.T) {
	var authorizations []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	provider := &fakeProvider{values: []aws.Credentials{
		{AccessKeyID: "FIRST", SecretAccessKey: "secret-1", CanExpire: true, Expires: time.Now().Add(-time.Minute)},
		{AccessKeyID: "SECOND", SecretAccessKey: "secret-2", CanExpire: false},
	}}
	transport := testTransport(t, "https://bedrock-runtime.us-east-2.amazonaws.com", provider, client)

	for i := 0; i < 2; i++ {
		request, err := http.NewRequest(http.MethodPost, "https://attacker.invalid/model/x/invoke", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.Do(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if provider.Calls() != 2 {
		t.Fatalf("provider calls = %d, want replacement retrieval on second call", provider.Calls())
	}
	if !strings.Contains(authorizations[0], "Credential=FIRST/") || !strings.Contains(authorizations[1], "Credential=SECOND/") {
		t.Fatalf("authorizations did not rotate credentials: %v", authorizations)
	}
}

func TestDoStripsClientAuthenticationAWSAndHopByHopHeaders(t *testing.T) {
	var got *http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Connection": []string{"X-Response-Hop"}, "X-Response-Hop": []string{"secret"}, "Keep-Alive": []string{"timeout=5"}},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})}
	transport := testTransport(t, "https://bedrock-runtime.us-east-2.amazonaws.com", &fakeProvider{values: []aws.Credentials{{AccessKeyID: "A", SecretAccessKey: "S"}}}, client)
	request, err := http.NewRequest(http.MethodPost, "https://client.example.invalid/model/x/invoke", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer client-secret")
	request.Header.Set("X-Api-Key", "client-secret")
	request.Header.Set("X-Amz-Date", "old")
	request.Header.Set("X-Amz-Security-Token", "old")
	request.Header.Set("X-Amz-Target", "old")
	request.Header.Set("X-Bedrock-Proxy-Catalog", "local-metadata")
	request.Header.Set("X-Bedrock-Proxy-Claude-Settings", "local-settings")
	request.Header.Set("Connection", "X-Remove, keep-alive")
	request.Header.Set("X-Remove", "must disappear")
	request.Header.Set("Content-Length", "999")
	response, err := transport.Do(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !strings.HasPrefix(got.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Fatalf("upstream Authorization was not generated by SigV4: %q", got.Header.Get("Authorization"))
	}
	for _, name := range []string{"X-Api-Key", "X-Amz-Target", "X-Bedrock-Proxy-Catalog", "X-Bedrock-Proxy-Claude-Settings", "X-Remove", "Connection", "Keep-Alive", "Content-Length"} {
		if got.Header.Get(name) != "" {
			t.Errorf("upstream retained %s = %q", name, got.Header.Get(name))
		}
	}
	if got.Header.Get("X-Amz-Date") == "old" || got.Header.Get("X-Amz-Date") == "" {
		t.Error("upstream did not replace client signing date")
	}
	if got.Header.Get("X-Amz-Security-Token") == "old" {
		t.Error("upstream retained client security token")
	}
	if got.Host != "bedrock-runtime.us-east-2.amazonaws.com" {
		t.Fatalf("upstream Host = %q, want configured authority", got.Host)
	}
	if response.Header.Get("Connection") != "" || response.Header.Get("X-Response-Hop") != "" || response.Header.Get("Keep-Alive") != "" {
		t.Fatalf("hop-by-hop response headers were not stripped: %v", response.Header)
	}
}

func TestDoStripsNonCanonicalSensitiveHeaders(t *testing.T) {
	var got *http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	transport := testTransport(t, "https://bedrock-runtime.us-east-2.amazonaws.com", &fakeProvider{values: []aws.Credentials{{AccessKeyID: "A", SecretAccessKey: "S"}}}, client)
	request, err := http.NewRequest(http.MethodPost, "https://client.example.invalid/model/x/invoke", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header["authorization"] = []string{"client-secret"}
	request.Header["x-api-key"] = []string{"client-secret"}
	request.Header["x-amz-date"] = []string{"old-date"}
	if _, err := transport.Do(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for key, values := range got.Header {
		if strings.EqualFold(key, "x-api-key") || strings.EqualFold(key, "x-amz-date") {
			for _, value := range values {
				if value == "client-secret" || value == "old-date" {
					t.Fatalf("forwarded noncanonical sensitive header %q=%q", key, value)
				}
			}
		}
		if strings.EqualFold(key, "authorization") {
			for _, value := range values {
				if value == "client-secret" {
					t.Fatalf("forwarded noncanonical authorization header %q=%q", key, value)
				}
			}
		}
	}
}

func TestDoIgnoresClientDestinationAndBlocksRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != strings.TrimPrefix(origin.URL, "http://") {
			t.Errorf("origin Host = %q, want fixed authority", r.Host)
		}
		http.Redirect(w, r, target.URL+"/leak", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	transport := testTransport(t, origin.URL, &fakeProvider{values: []aws.Credentials{{AccessKeyID: "A", SecretAccessKey: "S"}}}, origin.Client())
	request, err := http.NewRequest(http.MethodPost, target.URL+"/client-path", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.Do(context.Background(), request)
	if ClassOf(err) != FailureRedirect {
		t.Fatalf("redirect error class = %q, want %q (err=%v)", ClassOf(err), FailureRedirect, err)
	}
	if redirected {
		t.Fatal("redirect target received a request")
	}
}

func TestDoPropagatesCancellationAndClassifiesFailures(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	transport := testTransport(t, "https://bedrock-runtime.us-east-2.amazonaws.com", &fakeProvider{values: []aws.Credentials{{AccessKeyID: "A", SecretAccessKey: "S"}}}, client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://client.example.invalid/model/x/invoke", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.Do(ctx, request)
	if ClassOf(err) != FailureCanceled {
		t.Fatalf("cancellation class = %q, want %q (err=%v)", ClassOf(err), FailureCanceled, err)
	}

	connectivity := testTransport(t, "https://bedrock-runtime.us-east-2.amazonaws.com", &fakeProvider{values: []aws.Credentials{{AccessKeyID: "A", SecretAccessKey: "S"}}}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") })})
	request, _ = http.NewRequest(http.MethodPost, "https://client.example.invalid/model/x/invoke", strings.NewReader("{}"))
	_, err = connectivity.Do(context.Background(), request)
	if ClassOf(err) != FailureConnectivity {
		t.Fatalf("connectivity class = %q, want %q (err=%v)", ClassOf(err), FailureConnectivity, err)
	}
}

func TestCredentialFailureDoesNotExposeProviderError(t *testing.T) {
	secret := "AKIA-secret-value"
	transport := testTransport(t, "https://bedrock-runtime.us-east-2.amazonaws.com", &fakeProvider{values: nil}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("request should not be sent"); return nil, nil })})
	transport.creds = aws.NewCredentialsCache(errorProvider{err: errors.New("expired SSO token AKIA-secret-value")})
	request, _ := http.NewRequest(http.MethodPost, "https://client.example.invalid/model/x/invoke", strings.NewReader("{}"))
	_, err := transport.Do(context.Background(), request)
	if ClassOf(err) != FailureCredentialsExpired {
		t.Fatalf("credential class = %q, want %q", ClassOf(err), FailureCredentialsExpired)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("credential error exposed secret: %q", err)
	}
}

type errorProvider struct{ err error }

func (p errorProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{}, p.err
}
