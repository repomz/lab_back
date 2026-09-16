package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDeepSeekRequestsPerMinute = 30
	defaultDeepSeekRequestsPerHour   = 300
	defaultDeepSeekMaxConcurrent     = 2
	defaultDeepSeekTimeout           = 45 * time.Second
	maxDeepSeekRequestBytes          = 256 << 10
	maxDeepSeekResponseBytes         = 1 << 20
)

var (
	ErrDeepSeekRateLimited = errors.New("deepseek rate limited")
	ErrDeepSeekBusy        = errors.New("deepseek concurrency limit reached")
)

type deepSeekLimitError struct {
	retryAfter time.Duration
}

func (e *deepSeekLimitError) Error() string { return ErrDeepSeekRateLimited.Error() }
func (e *deepSeekLimitError) Unwrap() error { return ErrDeepSeekRateLimited }

func DeepSeekRetryAfter(err error) (time.Duration, bool) {
	var limitErr *deepSeekLimitError
	if errors.As(err, &limitErr) {
		return positiveRetryAfter(limitErr.retryAfter), true
	}
	return 0, false
}

type deepSeekLimiter struct {
	mu            sync.Mutex
	windowStarted time.Time
	used          int
	hourStarted   time.Time
	hourUsed      int
	perMinute     int
	perHour       int
	slots         chan struct{}
}

func newDeepSeekLimiter(perMinute, perHour, maxConcurrent int) *deepSeekLimiter {
	if perMinute <= 0 {
		perMinute = defaultDeepSeekRequestsPerMinute
	}
	if maxConcurrent <= 0 {
		maxConcurrent = defaultDeepSeekMaxConcurrent
	}
	if perHour <= 0 {
		perHour = defaultDeepSeekRequestsPerHour
	}
	if perHour < perMinute {
		perHour = perMinute
	}
	return &deepSeekLimiter{perMinute: perMinute, perHour: perHour, slots: make(chan struct{}, maxConcurrent)}
}

func (l *deepSeekLimiter) acquire(now time.Time) (func(), error) {
	select {
	case l.slots <- struct{}{}:
	default:
		return nil, ErrDeepSeekBusy
	}
	release := func() { <-l.slots }

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windowStarted.IsZero() || now.Sub(l.windowStarted) >= time.Minute {
		l.windowStarted, l.used = now, 0
	}
	if l.hourStarted.IsZero() || now.Sub(l.hourStarted) >= time.Hour {
		l.hourStarted, l.hourUsed = now, 0
	}
	if l.used >= l.perMinute {
		release()
		return nil, &deepSeekLimitError{retryAfter: time.Minute - now.Sub(l.windowStarted)}
	}
	if l.hourUsed >= l.perHour {
		release()
		return nil, &deepSeekLimitError{retryAfter: time.Hour - now.Sub(l.hourStarted)}
	}
	l.used++
	l.hourUsed++
	return release, nil
}

func positiveRetryAfter(value time.Duration) time.Duration {
	if value < time.Second {
		return time.Second
	}
	return value
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return time.Minute
}

func (s *Service) requestDeepSeek(ctx context.Context, payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if len(body) > maxDeepSeekRequestBytes {
		return "", fmt.Errorf("deepseek request exceeds %d bytes", maxDeepSeekRequestBytes)
	}

	release, err := s.aiLimiter.acquire(time.Now().UTC())
	if err != nil {
		return "", err
	}
	defer release()

	timeout := time.Duration(s.cfg.DeepSeekTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultDeepSeekTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, strings.TrimRight(s.cfg.DeepSeekBaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.DeepSeekAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", &deepSeekLimitError{retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("deepseek status %d", resp.StatusCode)
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxDeepSeekResponseBytes))
	if err = decoder.Decode(&envelope); err != nil || len(envelope.Choices) == 0 || strings.TrimSpace(envelope.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("invalid deepseek response")
	}
	return envelope.Choices[0].Message.Content, nil
}
