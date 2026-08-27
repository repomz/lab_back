package analyzer

import "testing"

func TestParseMarkers(t *testing.T) {
	m := parseMarkers("Глюкоза 6,2 ммоль/л 3,9 - 5,5\nГемоглобин 140 г/л 120 - 160")
	if len(m) != 2 {
		t.Fatalf("got %d", len(m))
	}
	if string(m[0].Status) != "high" {
		t.Fatalf("status %s", m[0].Status)
	}
	if string(m[1].Status) != "normal" {
		t.Fatalf("status %s", m[1].Status)
	}
}

func TestEmptyReviewDoesNotClaimRecognition(t *testing.T) {
	r := emptyReview()
	if r.Summary == "" || r.Provider != "rules" {
		t.Fatalf("unexpected review: %#v", r)
	}
}
