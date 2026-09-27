package store

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestUploadedArticleMediaPath(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "uploaded image", url: "/api/v1/articles/media/cover.webp", want: "/uploads/article-media/cover.webp"},
		{name: "external image", url: "https://example.org/cover.webp", want: ""},
		{name: "unrelated API", url: "/api/v1/analyses/file", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uploadedArticleMediaPath("/uploads", tt.url); got != tt.want {
				t.Fatalf("uploadedArticleMediaPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestArticleVisibilityFilterOnlyIncludesOwnDrafts(t *testing.T) {
	viewer := primitive.NewObjectID()
	filter := articleVisibilityFilter(viewer, true)
	clauses, ok := filter["$or"].([]primitive.M)
	if !ok || len(clauses) != 2 {
		t.Fatalf("unexpected doctor article filter: %#v", filter)
	}
	if clauses[0]["published"] != true || clauses[1]["doctor_id"] != viewer {
		t.Fatalf("doctor filter exposes another doctor's drafts: %#v", filter)
	}

	public := articleVisibilityFilter(viewer, false)
	if len(public) != 1 || public["published"] != true {
		t.Fatalf("unexpected public article filter: %#v", public)
	}
}
