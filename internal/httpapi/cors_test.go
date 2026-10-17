package httpapi

import (
	"github.com/repomz/lab_back/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSAllowsSchedulePUT(t *testing.T) {
	api := &API{cfg: config.Config{CORSOrigins: []string{"https://www.ailab.su"}}}
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/doctor/schedule", nil)
	request.Header.Set("Origin", "https://www.ailab.su")
	request.Header.Set("Access-Control-Request-Method", "PUT")
	response := httptest.NewRecorder()
	api.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("preflight reached handler") })).ServeHTTP(response, request)
	if response.Code != 204 || !strings.Contains(response.Header().Get("Access-Control-Allow-Methods"), "PUT") {
		t.Fatalf("preflight rejected: %v", response.Result())
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://www.ailab.su" {
		t.Fatal("allowed origin missing")
	}
	request.Header.Set("Origin", "https://untrusted.example")
	response = httptest.NewRecorder()
	api.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(response, request)
	if response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("untrusted origin allowed")
	}
}
