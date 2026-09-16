package analyzer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/repomz/lab_back/internal/config"
)

func deepSeekTestService(serverURL string, rpm, concurrent int) *Service {
	return New(config.Config{
		DeepSeekAPIKey:            "test",
		DeepSeekBaseURL:           serverURL,
		DeepSeekModel:             "test",
		DeepSeekRequestsPerMinute: rpm,
		DeepSeekRequestsPerHour:   100,
		DeepSeekMaxConcurrent:     concurrent,
		DeepSeekTimeoutSeconds:    5,
	})
}

func TestDeepSeekLimiterEnforcesGlobalHourlyQuota(t *testing.T) {
	limiter := newDeepSeekLimiter(2, 2, 1)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		release, err := limiter.acquire(now.Add(time.Duration(i) * time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if _, err := limiter.acquire(now.Add(2 * time.Minute)); !errors.Is(err, ErrDeepSeekRateLimited) {
		t.Fatalf("hourly request error = %v, want rate limit", err)
	}
	release, err := limiter.acquire(now.Add(time.Hour))
	if err != nil {
		t.Fatalf("hour window must reset: %v", err)
	}
	release()
}

func requestTestJSON(service *Service) error {
	var out struct {
		Answer string `json:"answer"`
	}
	return service.completeJSON(context.Background(), "system", "user", &out)
}

func TestDeepSeekLimiterEnforcesGlobalRPM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"answer\":\"ok\"}"}}]}`))
	}))
	defer server.Close()
	service := deepSeekTestService(server.URL, 1, 1)

	if err := requestTestJSON(service); err != nil {
		t.Fatal(err)
	}
	err := requestTestJSON(service)
	if !errors.Is(err, ErrDeepSeekRateLimited) {
		t.Fatalf("second request error = %v, want rate limit", err)
	}
	if retry, ok := DeepSeekRetryAfter(err); !ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("retry after = %s, ok=%v", retry, ok)
	}
}

func TestDeepSeekLimiterRejectsExcessConcurrency(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"answer\":\"ok\"}"}}]}`))
	}))
	defer server.Close()
	service := deepSeekTestService(server.URL, 10, 1)
	firstDone := make(chan error, 1)
	go func() { firstDone <- requestTestJSON(service) }()
	<-started

	if err := requestTestJSON(service); !errors.Is(err, ErrDeepSeekBusy) {
		t.Fatalf("concurrent request error = %v, want busy", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestDeepSeekUpstream429PreservesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	service := deepSeekTestService(server.URL, 10, 1)

	err := requestTestJSON(service)
	if !errors.Is(err, ErrDeepSeekRateLimited) {
		t.Fatalf("error = %v, want upstream rate limit", err)
	}
	if retry, ok := DeepSeekRetryAfter(err); !ok || retry != 17*time.Second {
		t.Fatalf("retry after = %s, ok=%v", retry, ok)
	}
}
