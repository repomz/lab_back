package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/repomz/lab_back/internal/domain"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Clinical content, not updated_at: sharing a report must not spend another
// provider request. Only completed studies participate in a health summary.
func HealthSummaryKey(analyses []domain.Analysis) string {
	items := completedAnalyses(analyses)
	sort.Slice(items, func(i, j int) bool { return items[i].ID.Hex() < items[j].ID.Hex() })
	clinical := make([]any, 0, len(items))
	for _, item := range items {
		clinical = append(clinical, struct {
			ID                       primitive.ObjectID
			Title, Category, OCRText string
			Date                     time.Time
			Markers                  []domain.Marker
			Report                   *domain.StudyReport
			Review                   domain.AIReview
		}{item.ID, item.Title, item.Category, item.OCRText, summaryDate(item), item.Markers, item.Report, item.AIReview})
	}
	data, _ := json.Marshal(clinical)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func summaryDate(item domain.Analysis) time.Time {
	if item.CollectedAt != nil {
		return *item.CollectedAt
	}
	return item.CreatedAt
}
