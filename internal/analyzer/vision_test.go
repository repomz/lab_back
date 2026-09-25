package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
)

func TestAnalyzeDocumentSendsOriginalImageAndPreservesPrintedReference(t *testing.T) {
	var imageWasSent bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) > 1 && strings.Contains(string(request.Messages[1].Content), "data:image/png;base64,") {
			imageWasSent = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"document_type\":\"laboratory\",\"title\":\"Общий анализ крови\",\"category\":\"Кровь · общий анализ\",\"collected_at\":\"2026-07-30\",\"medical_text\":\"СОЭ 21,0 мм/час; референс 2-15\",\"markers\":[{\"name\":\"СОЭ по Панченкову\",\"canonical_name\":\"esr\",\"value\":21.0,\"text_value\":\"\",\"unit\":\"мм/час\",\"reference_min\":2,\"reference_max\":15,\"reference_text\":\"2-15\",\"status\":\"high\"}],\"report\":null}"}}]}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "analysis.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nplaceholder"), 0600); err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{
		DeepSeekAPIKey: "test", DeepSeekBaseURL: server.URL, DeepSeekModel: "test", DeepSeekVisionModel: "test",
		DeepSeekRequestsPerMinute: 10, DeepSeekRequestsPerHour: 100, DeepSeekMaxConcurrent: 1,
		DeepSeekTimeoutSeconds: 5, DeepSeekVisionTimeoutSeconds: 5, OCRWorkerCount: 1,
	})
	result, err := service.AnalyzeDocumentForPatient(context.Background(), path, "image/png", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !imageWasSent {
		t.Fatal("multimodal request did not contain the original image")
	}
	if len(result.Markers) != 1 || result.Markers[0].ReferenceMin == nil || *result.Markers[0].ReferenceMin != 2 || result.Markers[0].ReferenceMax == nil || *result.Markers[0].ReferenceMax != 15 {
		t.Fatalf("printed reference was not preserved: %#v", result.Markers)
	}
	if result.Markers[0].Status != domain.StatusHigh {
		t.Fatalf("expected high status from the printed range, got %s", result.Markers[0].Status)
	}
}

func TestSanitizeMedicalTextDropsPatientIdentifiers(t *testing.T) {
	input := "ФИО пациента: Иванова Анна, Дата рождения: 01.01.1980, Номер медицинской карты 42\nСОЭ 21 мм/час\nАдрес: Томск"
	got := sanitizeMedicalText(input)
	if got != "СОЭ 21 мм/час" {
		t.Fatalf("unexpected retained text: %q", got)
	}
}
