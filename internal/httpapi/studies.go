package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/repomz/lab_back/internal/analyzer"
	"github.com/repomz/lab_back/internal/domain"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Each study owns its original file, so deleting one never breaks its siblings.
func copyStudyOriginal(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return nil
}

func applyStudyResult(item domain.Analysis, result analyzer.DocumentResult, index, count int) domain.Analysis {
	item.Title, item.Category, item.CollectedAt = result.Title, result.Category, result.CollectedAt
	item.OCRText, item.Markers, item.Report, item.AIReview = result.MedicalText, result.Markers, result.Report, result.Review
	item.Status, item.ProcessingStage, item.ProcessingProgress = domain.AnalysisStatusReady, domain.ProcessingStageCompleted, 100
	item.ProcessingError = ""
	item.SourceStudyIndex, item.SourceStudyCount = index, count
	item.RelatedAnalyses = nil
	return item
}

func (a *API) saveStudies(ctx context.Context, base domain.Analysis, results []analyzer.DocumentResult, replace bool) (domain.Analysis, error) {
	return persistStudies(ctx, a.store, base, results, replace)
}

type studyStore interface {
	CreateAnalysis(context.Context, *domain.Analysis) error
	DeleteAnalysis(context.Context, primitive.ObjectID, primitive.ObjectID) error
	ReplaceAnalysisResult(context.Context, primitive.ObjectID, primitive.ObjectID, domain.Analysis) error
}

func persistStudies(ctx context.Context, db studyStore, base domain.Analysis, results []analyzer.DocumentResult, replace bool) (domain.Analysis, error) {
	if len(results) == 0 {
		return domain.Analysis{}, fmt.Errorf("no studies")
	}
	items := make([]domain.Analysis, len(results))
	paths := []string{}
	if !replace {
		paths = append(paths, base.StoragePath)
	}
	cleanup := func() {
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}
	for i, result := range results {
		item := applyStudyResult(base, result, i, len(results))
		if i > 0 {
			item.ID = primitive.NewObjectID()
			item.StoragePath = filepath.Join(filepath.Dir(base.StoragePath), item.ID.Hex()+filepath.Ext(base.StoragePath))
			if err := copyStudyOriginal(base.StoragePath, item.StoragePath); err != nil {
				cleanup()
				return domain.Analysis{}, err
			}
			paths = append(paths, item.StoragePath)
		}
		items[i] = item
	}
	// Standalone MongoDB has no multi-document transactions. Compensate failed
	// writes using exact new IDs and a fresh context, even after HTTP cancellation.
	created := []primitive.ObjectID{}
	rollback := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		allRemoved := true
		for _, id := range created {
			if err := db.DeleteAnalysis(cleanupCtx, id, base.OwnerID); err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
				allRemoved = false
			}
		}
		// Keep originals if the database outcome is uncertain.
		if allRemoved {
			cleanup()
		}
	}
	start := 0
	if replace {
		start = 1
	}
	for i := start; i < len(items); i++ {
		created = append(created, items[i].ID)
		if err := db.CreateAnalysis(ctx, &items[i]); err != nil {
			rollback()
			return domain.Analysis{}, err
		}
	}
	if replace {
		if err := db.ReplaceAnalysisResult(ctx, base.ID, base.OwnerID, items[0]); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				rollback()
			}
			// On an ambiguous update failure, keep siblings: the parent may have
			// committed. They remain valid independent results with original files.
			return domain.Analysis{}, err
		}
	}
	items[0].UpdatedAt = time.Now().UTC()
	items[0].RelatedAnalyses = items[1:]
	return items[0], nil
}

// Reprocessing an already separated study must not recreate its siblings or
// rely on provider array order. Require a unique type/marker identity match.
func selectStudy(previous domain.Analysis, results []analyzer.DocumentResult) (analyzer.DocumentResult, error) {
	var matches []analyzer.DocumentResult
	for _, result := range results {
		if result.Title != previous.Title || result.Category != previous.Category {
			continue
		}
		if previous.CollectedAt != nil && result.CollectedAt != nil && !previous.CollectedAt.Equal(*result.CollectedAt) {
			continue
		}
		matches = append(matches, result)
	}
	if len(matches) != 1 {
		return analyzer.DocumentResult{}, fmt.Errorf("не удалось однозначно сопоставить исследование с исходным документом; сохранённые данные не изменены")
	}
	return matches[0], nil
}
