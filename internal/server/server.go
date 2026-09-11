// Package server provides the local HTTP surface that does not require AWS.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

var modelCreatedUnix = time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC).Unix()

type Server struct {
	cfg                config.Config
	http               *http.Server
	transport          RequestDoer
	record             func(CompletionResult)
	lifecycleMu        sync.Mutex
	active             map[uint64]context.CancelFunc
	activeChanged      chan struct{}
	nextRequestID      uint64
	stopping           bool
	pendingCompletions int
}

func New(cfg config.Config) *Server {
	s := &Server{
		cfg:           cfg,
		transport:     &lazyTransport{profile: cfg.AWS.Profile, region: cfg.AWS.Region},
		active:        make(map[uint64]context.CancelFunc),
		activeChanged: make(chan struct{}),
	}
	s.http = &http.Server{Handler: s}
	return s
}

// lazyTransport keeps local startup independent of AWS session state. The
// SDK provider chain is loaded on the first generation request, and the SDK
// retrieves credentials only when that request is signed.
type lazyTransport struct {
	profile string
	region  string
	mu      sync.Mutex
	inner   RequestDoer
}

func (l *lazyTransport) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	l.mu.Lock()
	if l.inner == nil {
		inner, err := transport.New(context.Background(), l.profile, l.region)
		if err != nil {
			l.mu.Unlock()
			return nil, err
		}
		l.inner = inner
	}
	inner := l.inner
	l.mu.Unlock()
	return inner.Do(ctx, request)
}

// NewWithTransport constructs a server with the shared AWS transport (or a
// local contract fake). Keeping the transport behind this small interface
// lets endpoint tests use the same request and response shapes without AWS
// credentials or a network connection.
func NewWithTransport(cfg config.Config, transport RequestDoer) *Server {
	s := New(cfg)
	s.transport = transport
	return s
}

// SetCompletionRecorder installs the metadata-only completion callback used
// by request accounting. It must be configured before serving requests.
func (s *Server) SetCompletionRecorder(record func(CompletionResult)) {
	s.record = record
}

func (s *Server) Handler() http.Handler { return s }

func (s *Server) Serve(l net.Listener) error { return s.http.Serve(l) }

// PendingCompletions reports accepted requests whose completion callback has
// not returned. It is used only to disclose incomplete accounting coverage
// after the bounded shutdown grace.
func (s *Server) PendingCompletions() int {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.pendingCompletions
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.stopping = true
	s.lifecycleMu.Unlock()
	shutdownErr := s.http.Shutdown(ctx)
	for {
		s.lifecycleMu.Lock()
		if len(s.active) == 0 {
			s.lifecycleMu.Unlock()
			return shutdownErr
		}
		changed := s.activeChanged
		s.lifecycleMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			s.cancelActiveRequests()
			// Cancellation is the final drain step. Wait for every handler to
			// publish its completion before the caller emits session totals.
			grace := time.NewTimer(250 * time.Millisecond)
			defer grace.Stop()
			for {
				s.lifecycleMu.Lock()
				if len(s.active) == 0 {
					s.lifecycleMu.Unlock()
					return shutdownErr
				}
				changed = s.activeChanged
				s.lifecycleMu.Unlock()
				select {
				case <-changed:
				case <-grace.C:
					return shutdownErr
				}
			}
		}
	}
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelRecord `json:"data"`
}

type modelRecord struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Owner   string `json:"owned_by"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID, requestContext, accepted := s.beginRequest(r)
	if !accepted {
		status := http.StatusServiceUnavailable
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"proxy is shutting down","type":"server_error"}}`))
		s.finishCompletion(started, CompletionResult{Endpoint: r.URL.Path, HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}
	defer s.endRequest(requestID)
	*r = *r.WithContext(requestContext)

	if r.URL.Path == "/v1/chat/completions" {
		s.serveChatCompletions(w, r)
		return
	}
	if r.URL.Path == "/v1/responses" {
		s.serveResponses(w, r)
		return
	}
	if r.URL.Path == "/v1/messages" {
		s.serveMessages(w, r)
		return
	}
	if r.URL.Path == "/v1/models" {
		s.serveModelList(w, r, started)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/models/") {
		s.serveModel(w, r, started, strings.TrimPrefix(r.URL.Path, "/v1/models/"))
		return
	}
	status := http.StatusNotFound
	http.NotFound(w, r)
	s.finishCompletion(started, CompletionResult{Endpoint: r.URL.Path, HTTPStatus: &status, Outcome: CompletionFailed})
}

func (s *Server) serveModelList(w http.ResponseWriter, r *http.Request, started time.Time) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		status := http.StatusMethodNotAllowed
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/models", HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}
	names := make([]string, 0, len(s.cfg.Models))
	for name := range s.cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	data := make([]modelRecord, 0, len(names))
	for _, name := range names {
		data = append(data, modelRecord{ID: name, Object: "model", Created: modelCreatedUnix, Owner: "bedrock-local-proxy"})
	}
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	outcome := CompletionSucceeded
	if err := json.NewEncoder(w).Encode(modelList{Object: "list", Data: data}); err != nil {
		outcome = CompletionFailed
	}
	s.finishCompletion(started, CompletionResult{Endpoint: "/v1/models", HTTPStatus: &status, Outcome: outcome})
}

func (s *Server) serveModel(w http.ResponseWriter, r *http.Request, started time.Time, alias string) {
	endpoint := "/v1/models/" + alias
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		status := http.StatusMethodNotAllowed
		w.WriteHeader(status)
		s.finishCompletion(started, CompletionResult{Endpoint: endpoint, LocalModel: alias, HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}
	if alias == "" {
		status := http.StatusNotFound
		s.writeOpenAIErrorDetails(w, status, "model not found", "invalid_request_error", "model", "model_not_found")
		s.finishCompletion(started, CompletionResult{Endpoint: endpoint, HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}
	if _, ok := s.cfg.Models[alias]; !ok {
		status := http.StatusNotFound
		s.writeOpenAIErrorDetails(w, status, fmt.Sprintf("model %q not found", alias), "invalid_request_error", "model", "model_not_found")
		s.finishCompletion(started, CompletionResult{Endpoint: endpoint, LocalModel: alias, HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	outcome := CompletionSucceeded
	if err := json.NewEncoder(w).Encode(modelRecord{ID: alias, Object: "model", Created: modelCreatedUnix, Owner: "bedrock-local-proxy"}); err != nil {
		outcome = CompletionFailed
	}
	s.finishCompletion(started, CompletionResult{Endpoint: endpoint, LocalModel: alias, HTTPStatus: &status, Outcome: outcome})
}

func (s *Server) beginRequest(r *http.Request) (uint64, context.Context, bool) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.stopping {
		// The rejection still emits a completion record, so reserve its
		// accounting slot before releasing the admission lock.
		s.pendingCompletions++
		return 0, nil, false
	}
	ctx, cancel := context.WithCancel(r.Context())
	s.nextRequestID++
	id := s.nextRequestID
	s.active[id] = cancel
	s.pendingCompletions++
	return id, ctx, true
}

func (s *Server) endRequest(id uint64) {
	s.lifecycleMu.Lock()
	if cancel, ok := s.active[id]; ok {
		delete(s.active, id)
		cancel()
		close(s.activeChanged)
		s.activeChanged = make(chan struct{})
	}
	s.lifecycleMu.Unlock()
}

func (s *Server) cancelActiveRequests() {
	s.lifecycleMu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.lifecycleMu.Unlock()
}
