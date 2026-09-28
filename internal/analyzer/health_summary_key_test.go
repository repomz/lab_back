package analyzer

import (
	"testing"
	"time"

	"github.com/repomz/lab_back/internal/domain"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestHealthSummaryKeyTracksContentNotSharingOrOrder(t *testing.T) {
	a := domain.Analysis{ID: primitive.NewObjectID(), Status: domain.AnalysisStatusReady, Title: "Blood", Markers: []domain.Marker{{Name: "HGB", TextValue: "130"}}}
	b := domain.Analysis{ID: primitive.NewObjectID(), Status: domain.AnalysisStatusReady, Title: "Urine"}
	key := HealthSummaryKey([]domain.Analysis{a, b})
	a.UpdatedAt = time.Now()
	a.SharedWith = []primitive.ObjectID{primitive.NewObjectID()}
	if key != HealthSummaryKey([]domain.Analysis{b, a}) {
		t.Fatal("sharing/order invalidated summary")
	}
	queued := domain.Analysis{ID: primitive.NewObjectID(), Status: domain.AnalysisStatusQueued}
	if key != HealthSummaryKey([]domain.Analysis{a, b, queued}) {
		t.Fatal("unfinished result invalidated summary")
	}
	queued.Status = domain.AnalysisStatusReady
	if key == HealthSummaryKey([]domain.Analysis{a, b, queued}) {
		t.Fatal("new result did not invalidate summary")
	}
	if key == HealthSummaryKey([]domain.Analysis{a}) {
		t.Fatal("deletion did not invalidate summary")
	}
	a.Markers[0].TextValue = "100"
	if key == HealthSummaryKey([]domain.Analysis{a, b}) {
		t.Fatal("corrected value did not invalidate summary")
	}
}
