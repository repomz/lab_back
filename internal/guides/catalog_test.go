package guides

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOfficialCatalogIntegration(t *testing.T) {
	if os.Getenv("LAB_GUIDES_INTEGRATION") == "" {
		t.Skip("set LAB_GUIDES_INTEGRATION=1 to call the official catalog")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, synced, err := New().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 10 || len(items) > 500 {
		t.Fatalf("catalog is unexpectedly small: %d", len(items))
	}
	if synced.IsZero() || items[0].Title == "" || len(items[0].Specialties) == 0 || items[0].Category == "Дети" {
		t.Fatal("catalog metadata was not parsed")
	}
	detail, err := New().Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Title == "" || len(detail.Sections) < 3 {
		t.Fatalf("guide content was not parsed: %#v", detail)
	}
}
