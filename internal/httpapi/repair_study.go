package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/repomz/lab_back/internal/analyzer"
	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
	"github.com/repomz/lab_back/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// RepairBloodUrine is an explicit operator migration for a reviewed legacy
// CBC/microalbumin record, never an automatic inference applied to patient data.
// It uses saved values only and makes no external AI requests. Back up MongoDB
// before apply; the original file remains available on both resulting records.
func RepairBloodUrine(ctx context.Context, db *store.Mongo, id primitive.ObjectID, apply bool) (int, error) {
	item, err := db.Analysis(ctx, id)
	if err != nil {
		return 0, err
	}
	if item.SourceStudyCount > 1 {
		return 0, fmt.Errorf("already separated")
	}
	if item.Report != nil {
		return 0, fmt.Errorf("not a laboratory record")
	}
	groups := [][]domain.Marker{{}, {}}
	allowed := map[string]bool{"esr_panchenkov": true, "neutrophils_band": true, "neutrophils_segmented": true, "eosinophils": true, "monocytes_manual": true, "lymphocytes": true, "wbc": true, "rbc": true, "hgb": true, "hct": true, "plt": true, "lym_percent": true, "mon_percent": true, "gran_percent": true}
	for _, marker := range item.Markers {
		key := strings.ToLower(marker.CanonicalName)
		if key == "microalbumin" {
			groups[1] = append(groups[1], marker)
		} else if allowed[key] {
			groups[0] = append(groups[0], marker)
		} else {
			return 0, fmt.Errorf("unreviewed marker identity %q", key)
		}
	}
	if len(groups[0]) == 0 || len(groups[1]) != 1 {
		return 0, fmt.Errorf("expected CBC and exactly one microalbumin marker")
	}
	if !apply {
		return 2, nil
	}
	service := analyzer.New(config.Config{}) // deliberately no provider credentials
	results := make([]analyzer.DocumentResult, 2)
	for i, markers := range groups {
		text := "Общий анализ крови"
		if i == 1 {
			text = "Исследование на микроальбуминурию"
		}
		for _, marker := range markers {
			value := marker.TextValue
			if marker.Value != nil {
				value = fmt.Sprint(*marker.Value)
			}
			text += fmt.Sprintf("\n%s: %s %s; референс: %s", marker.Name, value, marker.Unit, marker.ReferenceText)
		}
		title, category := analyzer.CanonicalAnalysisIdentity(markers, text, nil)
		results[i] = analyzer.DocumentResult{Title: title, Category: category, CollectedAt: item.CollectedAt, MedicalText: text, Markers: markers, Review: service.ReviewMarkersForPatient(ctx, markers, nil)}
	}
	now := time.Now().UTC()
	item.ProcessingCompletedAt = &now
	_, err = (&API{store: db}).saveStudies(ctx, item, results, true)
	return 2, err
}
