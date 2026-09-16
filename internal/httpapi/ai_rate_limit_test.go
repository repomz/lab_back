package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAIUserLimiterEnforcesMinuteAndHourQuotas(t *testing.T) {
	limiter := newAIUserLimiter(2, 3)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	if ok, _ := limiter.allow("user-a", now); !ok {
		t.Fatal("first request must be allowed")
	}
	if ok, _ := limiter.allow("user-a", now.Add(time.Second)); !ok {
		t.Fatal("second request must be allowed")
	}
	if ok, retry := limiter.allow("user-a", now.Add(2*time.Second)); ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("minute quota was not enforced: allowed=%v retry=%s", ok, retry)
	}
	if ok, _ := limiter.allow("user-b", now.Add(2*time.Second)); !ok {
		t.Fatal("quotas must be isolated by user")
	}
	if ok, _ := limiter.allow("user-a", now.Add(time.Minute)); !ok {
		t.Fatal("minute window must reset")
	}
	if ok, retry := limiter.allow("user-a", now.Add(time.Minute+time.Second)); ok || retry < 58*time.Minute {
		t.Fatalf("hour quota was not enforced: allowed=%v retry=%s", ok, retry)
	}
	if ok, _ := limiter.allow("user-a", now.Add(time.Hour)); !ok {
		t.Fatal("hour window must reset")
	}
}

func TestLimitAIRequestsReturns429AndRetryAfter(t *testing.T) {
	api := &API{aiLimiter: newAIUserLimiter(1, 10)}
	called := 0
	handler := api.limitAIRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	user := actor{ID: primitive.NewObjectID()}

	first := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ai/chats/id/messages", nil)
	handler.ServeHTTP(first, request.WithContext(withActor(request.Context(), user)))
	if first.Code != http.StatusNoContent {
		t.Fatalf("first status = %d", first.Code)
	}

	limited := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/ai/chats/id/messages", nil)
	handler.ServeHTTP(limited, request.WithContext(withActor(request.Context(), user)))
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("limited status=%d retry-after=%q", limited.Code, limited.Header().Get("Retry-After"))
	}
	if called != 1 {
		t.Fatalf("wrapped handler called %d times", called)
	}
}
