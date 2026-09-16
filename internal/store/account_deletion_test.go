package store

import "testing"

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
