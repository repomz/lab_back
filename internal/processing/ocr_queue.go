package processing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/repomz/lab_back/internal/analyzer"
	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type queueStore interface {
	RecoverExpiredOCRJobs(context.Context, int) error
	ClaimOCRJob(context.Context, string, time.Duration, int) (domain.Analysis, error)
	UpdateOCRJobProgress(context.Context, primitive.ObjectID, string, string, int, time.Duration) error
	CompleteOCRJob(context.Context, domain.Analysis, string, string, []domain.Marker, *domain.StudyReport, string, string, string, *time.Time) error
	RetryOCRJob(context.Context, primitive.ObjectID, string, time.Time, string, bool) error
	ReleaseOCRJob(context.Context, primitive.ObjectID, string) error
	UserByID(context.Context, primitive.ObjectID) (domain.User, error)
}

type recognizer interface {
	RecognizeJob(context.Context, string, string, *domain.PatientProfile, func(string, int)) (string, []domain.Marker, *domain.StudyReport, string, error)
}

type OCRQueue struct {
	store       queueStore
	recognizer  recognizer
	workers     int
	maxAttempts int
	timeout     time.Duration
	lease       time.Duration
	poll        time.Duration
	wg          sync.WaitGroup
}

func NewOCRQueue(cfg config.Config, store queueStore, recognizer recognizer) *OCRQueue {
	workers := cfg.OCRWorkerCount
	if workers <= 0 {
		workers = 1
	}
	maxAttempts := cfg.OCRJobMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	timeout := time.Duration(cfg.OCRJobTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	lease := time.Duration(cfg.OCRJobLeaseSeconds) * time.Second
	if lease <= timeout {
		lease = timeout + time.Minute
	}
	return &OCRQueue{store: store, recognizer: recognizer, workers: workers, maxAttempts: maxAttempts, timeout: timeout, lease: lease, poll: 500 * time.Millisecond}
}

func (q *OCRQueue) Start(ctx context.Context) {
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := q.store.RecoverExpiredOCRJobs(recoveryCtx, q.maxAttempts); err != nil {
		log.Printf("ocr queue recovery failed: %v", err)
	}
	cancel()

	host, _ := os.Hostname()
	for index := 0; index < q.workers; index++ {
		q.wg.Add(1)
		worker := fmt.Sprintf("%s-%d-%d", host, os.Getpid(), index+1)
		go q.runWorker(ctx, worker)
	}
	q.wg.Add(1)
	go q.runRecovery(ctx)
	log.Printf("ocr queue started workers=%d timeout=%s lease=%s attempts=%d", q.workers, q.timeout, q.lease, q.maxAttempts)
}

func (q *OCRQueue) Wait() { q.wg.Wait() }

func (q *OCRQueue) runRecovery(ctx context.Context) {
	defer q.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := q.store.RecoverExpiredOCRJobs(recoveryCtx, q.maxAttempts); err != nil {
				log.Printf("ocr queue recovery failed: %v", err)
			}
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

func (q *OCRQueue) runWorker(ctx context.Context, worker string) {
	defer q.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := q.store.ClaimOCRJob(ctx, worker, q.lease, q.maxAttempts)
		if errors.Is(err, mongo.ErrNoDocuments) {
			select {
			case <-time.After(q.poll):
				continue
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			log.Printf("ocr queue claim failed worker=%s: %v", worker, err)
			select {
			case <-time.After(2 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}
		q.processJob(ctx, worker, job)
	}
}

func (q *OCRQueue) processJob(parent context.Context, worker string, job domain.Analysis) {
	jobCtx, cancel := context.WithTimeout(parent, q.timeout)
	defer cancel()
	patient, err := q.store.UserByID(jobCtx, job.OwnerID)
	if err != nil {
		q.handleFailure(parent, worker, job, fmt.Errorf("load patient: %w", err))
		return
	}
	progress := func(stage string, value int) {
		if value < 0 {
			value = 0
		}
		if value > 99 {
			value = 99
		}
		if err := q.store.UpdateOCRJobProgress(jobCtx, job.ID, worker, stage, value, q.lease); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("ocr progress update failed analysis=%s: %v", job.ID.Hex(), err)
		}
	}
	text, markers, report, status, err := q.recognizer.RecognizeJob(jobCtx, job.StoragePath, job.MimeType, patient.PatientProfile, progress)
	if err != nil {
		q.handleFailure(parent, worker, job, err)
		return
	}
	category := analyzer.ClassifyAnalysis(markers, text)
	if err = q.store.CompleteOCRJob(jobCtx, job, worker, text, markers, report, status, category, category, analyzer.ExtractCollectedAt(text)); err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			log.Printf("ocr completion failed analysis=%s: %v", job.ID.Hex(), err)
		}
		return
	}
	log.Printf("ocr job complete analysis=%s attempt=%d markers=%d status=%s", job.ID.Hex(), job.ProcessingAttempt, len(markers), status)
}

func (q *OCRQueue) handleFailure(parent context.Context, worker string, job domain.Analysis, cause error) {
	operationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if parent.Err() != nil {
		if err := q.store.ReleaseOCRJob(operationCtx, job.ID, worker); err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			log.Printf("ocr job release failed analysis=%s: %v", job.ID.Hex(), err)
		}
		return
	}
	final := job.ProcessingAttempt >= q.maxAttempts
	message := "Не удалось распознать документ. Готовим повторную попытку."
	if final {
		message = "Не удалось распознать документ после нескольких попыток. Проверьте файл или загрузите более чёткую копию."
	}
	next := time.Now().UTC().Add(retryDelay(job.ProcessingAttempt))
	if err := q.store.RetryOCRJob(operationCtx, job.ID, worker, next, message, final); err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		log.Printf("ocr retry update failed analysis=%s: %v", job.ID.Hex(), err)
	}
	log.Printf("ocr job failed analysis=%s attempt=%d final=%v error=%v", job.ID.Hex(), job.ProcessingAttempt, final, cause)
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 5 * time.Second
	for index := 1; index < attempt; index++ {
		delay *= 3
		if delay >= 2*time.Minute {
			return 2 * time.Minute
		}
	}
	return delay
}
