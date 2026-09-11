package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

func TestShutdownRejectionPreservesPendingAcceptedCompletion(t *testing.T) {
	s := testServer()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	recorded := make(chan CompletionResult, 2)
	s.SetCompletionRecorder(func(result CompletionResult) {
		if result.HTTPStatus != nil && *result.HTTPStatus == http.StatusOK {
			close(entered)
			<-release
		}
		recorded <- result
	})
	go func() {
		defer close(done)
		s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	}()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("accepted handler did not finish")
		}
		if got := s.PendingCompletions(); got != 0 {
			t.Errorf("pending after accepted completion = %d, want 0", got)
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("accepted completion did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Shutdown(ctx)
	if got := s.PendingCompletions(); got != 1 {
		t.Fatalf("pending after bounded shutdown = %d, want 1", got)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("rejected status = %d, want 503", w.Code)
	}
	if got := s.PendingCompletions(); got != 1 {
		t.Errorf("pending after rejection = %d, want 1", got)
	}
	select {
	case result := <-recorded:
		if result.HTTPStatus == nil || *result.HTTPStatus != http.StatusServiceUnavailable || result.Outcome != CompletionFailed {
			t.Errorf("rejected completion = %+v", result)
		}
	default:
		t.Error("shutdown rejection was not recorded")
	}
}

func testServer() *Server {
	return New(config.Config{
		Models: map[string]config.ModelConfig{
			"fast":   {BedrockModelID: "model-a"},
			"coding": {BedrockModelID: "model-a"},
		},
	})
}

func TestModelsListsConfiguredPublicNamesWithoutUpstream(t *testing.T) {
	r := httptest.NewRecorder()
	testServer().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if r.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.Code)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "list" || len(got.Data) != 2 || got.Data[0].ID != "coding" || got.Data[1].ID != "fast" {
		t.Fatalf("unexpected model list: %+v", got)
	}
	if !strings.Contains(r.Body.String(), fmt.Sprintf(`"created":%d`, modelCreatedUnix)) || modelCreatedUnix <= 0 {
		t.Fatalf("model list lacks stable nonzero creation time: %s", r.Body.String())
	}
}

func TestModelRetrievalUsesStandardIdentityShape(t *testing.T) {
	r := httptest.NewRecorder()
	testServer().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/models/coding", nil))
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var model modelRecord
	if err := json.Unmarshal(r.Body.Bytes(), &model); err != nil {
		t.Fatal(err)
	}
	if model.ID != "coding" || model.Object != "model" || model.Created != modelCreatedUnix || model.Owner != "bedrock-local-proxy" {
		t.Fatalf("model=%+v", model)
	}
}

func TestModelsRejectsUnknownRoutesAndMethods(t *testing.T) {
	for _, tt := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPost, "/v1/models", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/models/coding", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/models/missing", http.StatusNotFound},
		{http.MethodGet, "/v1/unknown", http.StatusNotFound},
	} {
		r := httptest.NewRecorder()
		testServer().ServeHTTP(r, httptest.NewRequest(tt.method, tt.path, nil))
		if r.Code != tt.status {
			t.Errorf("%s %s status = %d, want %d", tt.method, tt.path, r.Code, tt.status)
		}
	}
}
