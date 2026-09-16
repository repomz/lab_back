package httpapi

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/repomz/lab_back/internal/analyzer"
)

const (
	defaultAIUserRequestsPerMinute = 6
	defaultAIUserRequestsPerHour   = 60
)

type aiUserWindow struct {
	minuteStarted time.Time
	hourStarted   time.Time
	lastSeen      time.Time
	minuteCount   int
	hourCount     int
}

type aiUserLimiter struct {
	mu          sync.Mutex
	users       map[string]aiUserWindow
	perMinute   int
	perHour     int
	lastCleanup time.Time
}

func newAIUserLimiter(perMinute, perHour int) *aiUserLimiter {
	if perMinute <= 0 {
		perMinute = defaultAIUserRequestsPerMinute
	}
	if perHour <= 0 {
		perHour = defaultAIUserRequestsPerHour
	}
	if perHour < perMinute {
		perHour = perMinute
	}
	return &aiUserLimiter{users: make(map[string]aiUserWindow), perMinute: perMinute, perHour: perHour}
}

func (l *aiUserLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	usage := l.users[key]
	if usage.minuteStarted.IsZero() || now.Sub(usage.minuteStarted) >= time.Minute {
		usage.minuteStarted, usage.minuteCount = now, 0
	}
	if usage.hourStarted.IsZero() || now.Sub(usage.hourStarted) >= time.Hour {
		usage.hourStarted, usage.hourCount = now, 0
	}
	usage.lastSeen = now

	if usage.minuteCount >= l.perMinute {
		l.users[key] = usage
		return false, positiveDuration(time.Minute - now.Sub(usage.minuteStarted))
	}
	if usage.hourCount >= l.perHour {
		l.users[key] = usage
		return false, positiveDuration(time.Hour - now.Sub(usage.hourStarted))
	}

	usage.minuteCount++
	usage.hourCount++
	l.users[key] = usage
	if l.lastCleanup.IsZero() || now.Sub(l.lastCleanup) >= time.Hour {
		for id, candidate := range l.users {
			if now.Sub(candidate.lastSeen) >= 2*time.Hour {
				delete(l.users, id)
			}
		}
		l.lastCleanup = now
	}
	return true, 0
}

func positiveDuration(value time.Duration) time.Duration {
	if value < time.Second {
		return time.Second
	}
	return value
}

func retryAfterSeconds(value time.Duration) string {
	return strconv.Itoa(int(math.Ceil(value.Seconds())))
}

func (a *API) limitAIRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := current(r)
		allowed, retryAfter := a.aiLimiter.allow(user.ID.Hex(), time.Now().UTC())
		if !allowed {
			w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
			w.Header().Set("Cache-Control", "no-store")
			write(w, http.StatusTooManyRequests, map[string]string{"error": "Слишком много запросов к AI. Подождите и повторите."})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeAIServiceError(w http.ResponseWriter, err error, status int, message string) {
	if retryAfter, limited := analyzer.DeepSeekRetryAfter(err); limited {
		w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
		w.Header().Set("Cache-Control", "no-store")
		write(w, http.StatusTooManyRequests, map[string]string{"error": "Лимит AI временно исчерпан. Повторите запрос позже."})
		return
	}
	if errors.Is(err, analyzer.ErrDeepSeekBusy) {
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Cache-Control", "no-store")
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "AI занят обработкой других запросов. Повторите немного позже."})
		return
	}
	write(w, status, map[string]string{"error": message})
}
