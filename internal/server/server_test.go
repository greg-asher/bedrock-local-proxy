package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

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
}

func TestModelsRejectsUnknownRoutesAndMethods(t *testing.T) {
	for _, tt := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPost, "/v1/models", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/unknown", http.StatusNotFound},
	} {
		r := httptest.NewRecorder()
		testServer().ServeHTTP(r, httptest.NewRequest(tt.method, tt.path, nil))
		if r.Code != tt.status {
			t.Errorf("%s %s status = %d, want %d", tt.method, tt.path, r.Code, tt.status)
		}
	}
}
