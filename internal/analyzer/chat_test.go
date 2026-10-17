package analyzer

import (
	"context"
	"encoding/json"
	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatUsesAudienceAndLimitsHistory(t *testing.T) {
	for _, role := range []domain.Role{domain.RolePatient, domain.RoleDoctor} {
		t.Run(string(role), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				prompt := body.Messages[0].Content
				if role == domain.RolePatient && !strings.Contains(prompt, "нет автоматического доступа") {
					t.Fatal("patient context limitation missing")
				}
				if role == domain.RoleDoctor && !strings.Contains(prompt, "помощник врача") {
					t.Fatal("doctor prompt missing")
				}
				if strings.Contains(body.Messages[1].Content, "old-message") {
					t.Fatal("unbounded chat history")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"reply\":\"Test response\"}"}}]}`))
			}))
			defer server.Close()
			service := New(config.Config{DeepSeekAPIKey: "test", DeepSeekBaseURL: server.URL, DeepSeekModel: "test"})
			messages := make([]domain.AIMessage, 21)
			messages[0] = domain.AIMessage{Role: "user", Content: "old-message"}
			reply, err := service.Chat(context.Background(), messages, role)
			if err != nil || reply != "Test response" {
				t.Fatalf("reply=%q err=%v", reply, err)
			}
		})
	}
}
