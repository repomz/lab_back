package httpapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/repomz/lab_back/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/sync/singleflight"
)

type summaryMemoryStore struct {
	mu     sync.Mutex
	values map[string]store.HealthSummary
}

func (s *summaryMemoryStore) HealthSummary(_ context.Context, owner primitive.ObjectID, key string) (store.HealthSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[owner.Hex()+key]
	if !ok {
		return value, mongo.ErrNoDocuments
	}
	return value, nil
}
func (s *summaryMemoryStore) SaveHealthSummary(_ context.Context, value store.HealthSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[value.OwnerID.Hex()+value.Fingerprint] = value
	return nil
}

func TestSummaryCachePersistsAndCoalescesRequests(t *testing.T) {
	db := &summaryMemoryStore{values: map[string]store.HealthSummary{}}
	owner := primitive.NewObjectID()
	var group singleflight.Group
	var calls atomic.Int32
	generate := func(context.Context) (string, error) { calls.Add(1); return "Saved response", nil }
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result, err := cachedHealthSummary(context.Background(), &group, db, owner, "same", generate)
			if err != nil || result.Summary != "Saved response" {
				t.Errorf("unexpected result: %#v %v", result, err)
			}
		}()
	}
	workers.Wait()
	// A new process/in-memory group must still read the persisted response.
	_, err := cachedHealthSummary(context.Background(), &singleflight.Group{}, db, owner, "same", generate)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("repeated provider call: %d %v", calls.Load(), err)
	}
	_, _ = cachedHealthSummary(context.Background(), &group, db, owner, "changed", generate)
	_, _ = cachedHealthSummary(context.Background(), &group, db, primitive.NewObjectID(), "same", generate)
	if calls.Load() != 3 {
		t.Fatalf("change/user isolation failed: %d", calls.Load())
	}
}

func TestSummaryFailureIsNotCached(t *testing.T) {
	db := &summaryMemoryStore{values: map[string]store.HealthSummary{}}
	var group singleflight.Group
	owner := primitive.NewObjectID()
	_, err := cachedHealthSummary(context.Background(), &group, db, owner, "same", func(context.Context) (string, error) { return "fallback", errors.New("provider failed") })
	if err == nil {
		t.Fatal("provider error lost")
	}
	if _, err = db.HealthSummary(context.Background(), owner, "same"); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatal("error response cached")
	}
	got, err := cachedHealthSummary(context.Background(), &group, db, owner, "same", func(context.Context) (string, error) { return "actual summary", nil })
	if err != nil || got.Summary != "actual summary" {
		t.Fatal("retry failed")
	}
}
