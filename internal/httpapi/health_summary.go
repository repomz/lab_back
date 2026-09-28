package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/repomz/lab_back/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/sync/singleflight"
)

type healthSummaryStore interface {
	HealthSummary(context.Context, primitive.ObjectID, string) (store.HealthSummary, error)
	SaveHealthSummary(context.Context, store.HealthSummary) error
}

type healthSummaryLimitError struct{ retryAfter time.Duration }

func (e *healthSummaryLimitError) Error() string { return "health summary request limit reached" }
func writeHealthSummaryError(w http.ResponseWriter, err error) {
	var limited *healthSummaryLimitError
	if errors.As(err, &limited) {
		w.Header().Set("Retry-After", retryAfterSeconds(limited.retryAfter))
		write(w, http.StatusTooManyRequests, map[string]string{"error": "Слишком много запросов к AI. Подождите и повторите."})
		return
	}
	writeAIServiceError(w, err, http.StatusServiceUnavailable, "Не удалось получить и сохранить резюме. Попробуйте ещё раз.")
}

// Cache is persistent and owner-scoped. Concurrent opens in this API process
// join the same generation. Closing a screen does not discard a paid response.
func cachedHealthSummary(ctx context.Context, group *singleflight.Group, db healthSummaryStore, owner primitive.ObjectID, key string, generate func(context.Context) (string, error)) (store.HealthSummary, error) {
	result, err := db.HealthSummary(ctx, owner, key)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return result, err
	}
	channel := group.DoChan(owner.Hex()+":"+key, func() (any, error) {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		cached, err := db.HealthSummary(work, owner, key)
		if err == nil {
			return cached, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, err
		}
		text, err := generate(work)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("empty health summary")
		}
		cached = store.HealthSummary{OwnerID: owner, Fingerprint: key, Summary: text, GeneratedAt: time.Now().UTC()}
		if err = db.SaveHealthSummary(work, cached); err != nil {
			return nil, err
		}
		return cached, nil
	})
	select {
	case <-ctx.Done():
		return store.HealthSummary{}, ctx.Err()
	case result := <-channel:
		if result.Err != nil {
			return store.HealthSummary{}, result.Err
		}
		return result.Val.(store.HealthSummary), nil
	}
}
