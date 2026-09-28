package httpapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/repomz/lab_back/internal/analyzer"
	"github.com/repomz/lab_back/internal/domain"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type memoryStudyStore struct {
	items   map[primitive.ObjectID]domain.Analysis
	creates int
	failAt  int
}

func (s *memoryStudyStore) CreateAnalysis(_ context.Context, a *domain.Analysis) error {
	s.creates++
	if s.creates == s.failAt {
		return fmt.Errorf("write failure")
	}
	s.items[a.ID] = *a
	return nil
}
func (s *memoryStudyStore) DeleteAnalysis(ctx context.Context, id, owner primitive.ObjectID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(s.items, id)
	return nil
}
func (s *memoryStudyStore) ReplaceAnalysisResult(_ context.Context, id, owner primitive.ObjectID, a domain.Analysis) error {
	s.items[id] = a
	return nil
}

func TestPersistStudiesOwnFilesAndRollback(t *testing.T) {
	for _, failAt := range []int{0, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			base := domain.Analysis{ID: primitive.NewObjectID(), OwnerID: primitive.NewObjectID(), StoragePath: filepath.Join(t.TempDir(), "original.pdf")}
			if err := os.WriteFile(base.StoragePath, []byte("source"), 0600); err != nil {
				t.Fatal(err)
			}
			db := &memoryStudyStore{items: map[primitive.ObjectID]domain.Analysis{}, failAt: failAt}
			got, err := persistStudies(context.Background(), db, base, []analyzer.DocumentResult{{Title: "Blood"}, {Title: "Urine"}}, false)
			if failAt > 0 {
				if err == nil || len(db.items) != 0 {
					t.Fatal("partial batch left after failed save")
				}
				files, _ := filepath.Glob(filepath.Join(filepath.Dir(base.StoragePath), "*"))
				if len(files) != 0 {
					t.Fatal("child file not cleaned up")
				}
				return
			}
			if err != nil || len(got.RelatedAnalyses) != 1 || len(db.items) != 2 {
				t.Fatalf("batch not saved: %v", err)
			}
			child := got.RelatedAnalyses[0]
			if child.StoragePath == got.StoragePath || child.SourceStudyIndex != 1 || got.SourceStudyCount != 2 {
				t.Fatal("invalid split metadata")
			}
			if err := os.Remove(got.StoragePath); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(child.StoragePath); err != nil {
				t.Fatal("sibling original removed")
			}
		})
	}
}

func TestStudyOriginalsAreIndependentAndNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.pdf"), filepath.Join(dir, "child.pdf")
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyStudyOriginal(source, destination); err != nil {
		t.Fatal(err)
	}
	if err := copyStudyOriginal(source, destination); err == nil {
		t.Fatal("overwrote existing file")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "original" {
		t.Fatalf("sibling lost its original: %s %v", data, err)
	}
}

func TestReprocessMatchesIdentityNotArrayPosition(t *testing.T) {
	previous := domain.Analysis{Title: "Общий анализ крови", Category: "Кровь"}
	blood := analyzer.DocumentResult{Title: previous.Title, Category: previous.Category}
	urine := analyzer.DocumentResult{Title: "Микроальбумин мочи", Category: "Моча"}
	got, err := selectStudy(previous, []analyzer.DocumentResult{urine, blood})
	if err != nil || got.Title != blood.Title {
		t.Fatalf("wrong study: %#v %v", got, err)
	}
	if _, err := selectStudy(previous, []analyzer.DocumentResult{blood, blood}); err == nil {
		t.Fatal("ambiguous match accepted")
	}
	if _, err := selectStudy(previous, []analyzer.DocumentResult{urine}); err == nil {
		t.Fatal("missing study replaced with sibling")
	}
}
