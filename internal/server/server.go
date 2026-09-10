// Package server provides the local HTTP surface that does not require AWS.
package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"sync"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

type Server struct {
	cfg       config.Config
	http      *http.Server
	transport RequestDoer
	record    func(CompletionResult)
}

func New(cfg config.Config) *Server {
	s := &Server{cfg: cfg, transport: &lazyTransport{profile: cfg.AWS.Profile, region: cfg.AWS.Region}}
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

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

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
	if r.URL.Path == "/v1/chat/completions" {
		s.serveChatCompletions(w, r)
		return
	}
	if r.URL.Path == "/v1/messages" {
		s.serveMessages(w, r)
		return
	}
	if r.URL.Path != "/v1/models" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	names := make([]string, 0, len(s.cfg.Models))
	for name := range s.cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	data := make([]modelRecord, 0, len(names))
	for _, name := range names {
		data = append(data, modelRecord{ID: name, Object: "model", Owner: "bedrock-local-proxy"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelList{Object: "list", Data: data})
}
