package httpapi

import (
	"testing"
	"time"

	"github.com/repomz/lab_back/internal/domain"
)

func TestDeletionGracePeriod(t *testing.T) {
	if got := deletionGracePeriod(domain.RolePatient); got != 7*24*time.Hour {
		t.Fatalf("patient grace period = %s, want 7 days", got)
	}
	if got := deletionGracePeriod(domain.RoleDoctor); got != 3*24*time.Hour {
		t.Fatalf("doctor grace period = %s, want 3 days", got)
	}
}

func TestDeletionExpired(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	exact := now
	future := now.Add(time.Second)

	tests := []struct {
		name string
		at   *time.Time
		want bool
	}{
		{name: "not scheduled", want: false},
		{name: "past", at: &past, want: true},
		{name: "exact deadline", at: &exact, want: true},
		{name: "future", at: &future, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deletionExpired(domain.User{DeletionScheduledFor: tt.at}, now); got != tt.want {
				t.Fatalf("deletionExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}
