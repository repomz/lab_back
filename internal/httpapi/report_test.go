package httpapi

import (
	"bytes"
	"testing"
	"time"

	"github.com/repomz/lab_back/internal/domain"
)

func TestBuildAnalysisPDF(t *testing.T) {
	value := 517.6
	min := 154.7
	max := 428.4
	payload, err := buildAnalysisPDF(domain.Analysis{
		Title:     "Биохимический анализ крови",
		CreatedAt: time.Date(2026, time.March, 30, 16, 18, 0, 0, time.UTC),
		Markers: []domain.Marker{{
			Name:         "Мочевая кислота",
			Value:        &value,
			Unit:         "мкмоль/л",
			ReferenceMin: &min,
			ReferenceMax: &max,
			Status:       domain.StatusHigh,
		}},
	})
	if err != nil {
		if _, _, fontErr := reportFonts(); fontErr != nil {
			t.Skip(fontErr)
		}
		t.Fatal(err)
	}
	if !bytes.HasPrefix(payload, []byte("%PDF-")) {
		t.Fatalf("unexpected PDF header: %q", payload[:minInt(10, len(payload))])
	}
	if len(payload) < 10_000 {
		t.Fatalf("PDF is unexpectedly small: %d bytes", len(payload))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
