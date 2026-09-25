package processing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/repomz/lab_back/internal/domain"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type fakeQueueStore struct {
	user          domain.User
	progress      []string
	completed     bool
	retried       bool
	final         bool
	completedWith domain.Analysis
}

func (f *fakeQueueStore) RecoverExpiredOCRJobs(context.Context, int) error { return nil }
func (f *fakeQueueStore) ClaimOCRJob(context.Context, string, time.Duration, int) (domain.Analysis, error) {
	return domain.Analysis{}, mongo.ErrNoDocuments
}
func (f *fakeQueueStore) UpdateOCRJobProgress(_ context.Context, _ primitive.ObjectID, _ string, stage string, _ int, _ time.Duration) error {
	f.progress = append(f.progress, stage)
	return nil
}
func (f *fakeQueueStore) CompleteOCRJob(_ context.Context, job domain.Analysis, _ string, _ string, _ []domain.Marker, _ *domain.StudyReport, _ string, title, category string, _ *time.Time) error {
	f.completed = true
	job.Title, job.Category = title, category
	f.completedWith = job
	return nil
}
func (f *fakeQueueStore) RetryOCRJob(_ context.Context, _ primitive.ObjectID, _ string, _ time.Time, _ string, final bool) error {
	f.retried, f.final = true, final
	return nil
}
func (f *fakeQueueStore) ReleaseOCRJob(context.Context, primitive.ObjectID, string) error { return nil }
func (f *fakeQueueStore) UserByID(context.Context, primitive.ObjectID) (domain.User, error) {
	return f.user, nil
}

type fakeRecognizer struct{ err error }

func (f fakeRecognizer) RecognizeJob(_ context.Context, _ string, _ string, _ *domain.PatientProfile, progress func(string, int)) (string, []domain.Marker, *domain.StudyReport, string, error) {
	progress(domain.ProcessingStageRecognizing, 35)
	if f.err != nil {
		return "", nil, nil, domain.AnalysisStatusFailed, f.err
	}
	value := 4.7
	return "Глюкоза 4,7 ммоль/л 3,9-6,4", []domain.Marker{{Name: "Глюкоза", CanonicalName: "glucose", Value: &value, Status: domain.StatusNormal}}, nil, domain.AnalysisStatusAwaitingConfirmation, nil
}

func TestOCRQueueCompletesRecognizedJob(t *testing.T) {
	store := &fakeQueueStore{user: domain.User{ID: primitive.NewObjectID(), Role: domain.RolePatient}}
	queue := &OCRQueue{store: store, recognizer: fakeRecognizer{}, maxAttempts: 3, timeout: time.Minute, lease: 2 * time.Minute}
	job := domain.Analysis{ID: primitive.NewObjectID(), OwnerID: store.user.ID, StoragePath: "/tmp/test.jpg", MimeType: "image/jpeg", ProcessingAttempt: 1}

	queue.processJob(context.Background(), "worker-1", job)
	if !store.completed || store.retried {
		t.Fatalf("completed=%v retried=%v", store.completed, store.retried)
	}
	if len(store.progress) == 0 || store.completedWith.Category == "" {
		t.Fatalf("progress=%v category=%q", store.progress, store.completedWith.Category)
	}
}

func TestOCRQueueRetriesTransientFailureAndStopsAtLimit(t *testing.T) {
	store := &fakeQueueStore{user: domain.User{ID: primitive.NewObjectID(), Role: domain.RolePatient}}
	queue := &OCRQueue{store: store, recognizer: fakeRecognizer{err: errors.New("tesseract failed")}, maxAttempts: 3, timeout: time.Minute, lease: 2 * time.Minute}
	job := domain.Analysis{ID: primitive.NewObjectID(), OwnerID: store.user.ID, StoragePath: "/tmp/test.jpg", MimeType: "image/jpeg", ProcessingAttempt: 2}
	queue.processJob(context.Background(), "worker-1", job)
	if !store.retried || store.final {
		t.Fatalf("retried=%v final=%v", store.retried, store.final)
	}
	store.retried = false
	job.ProcessingAttempt = 3
	queue.processJob(context.Background(), "worker-1", job)
	if !store.retried || !store.final {
		t.Fatalf("final attempt retried=%v final=%v", store.retried, store.final)
	}
}

func TestRetryDelayIsBounded(t *testing.T) {
	if got := retryDelay(1); got != 5*time.Second {
		t.Fatalf("first retry delay = %s", got)
	}
	if got := retryDelay(10); got != 2*time.Minute {
		t.Fatalf("bounded retry delay = %s", got)
	}
}
