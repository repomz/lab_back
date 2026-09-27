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

func TestMultipleStudiesKeepSpecimensAndReviewsSeparate(t *testing.T) {
	var extracted visionExtraction
	if err := json.Unmarshal([]byte(`{"studies":[
	{"medical_text":"Общий анализ крови. WBC 6,70; 4–10","markers":[{"name":"WBC","canonical_name":"WBC","value":6.7,"reference_min":4,"reference_max":10,"reference_text":"4–10"}]},
	{"medical_text":"Исследование на микроальбуминурию. Микроальбумин 0,60; 0–25 мг/сутки","markers":[{"name":"Микроальбумин","canonical_name":"Microalbumin","value":0.6,"unit":"мг/сутки","reference_min":0,"reference_max":25,"reference_text":"0–25"}]},
	{"medical_text":"УЗИ почек","report":{"modality":"УЗИ","study_name":"УЗИ почек","description":"Контуры чёткие, ровные.","conclusion":"Эхоскопически без выраженной патологии.","confidence":1}}
	]}`), &extracted); err != nil {
		t.Fatal(err)
	}
	results, err := New(config.Config{}).finishVisionStudies(context.Background(), extracted, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("lost studies: %d", len(results))
	}
	if results[0].Title != "Общий анализ крови" || results[1].Title != "Микроальбумин мочи" || results[2].Title != "УЗИ почек" {
		t.Fatalf("wrong identities: %#v", results)
	}
	if len(results[0].Markers) != 1 || len(results[1].Markers) != 1 || len(results[2].Markers) != 0 {
		t.Fatal("mixed study data")
	}
	if results[1].Markers[0].Value == nil || *results[1].Markers[0].Value != 0.6 || *results[1].Markers[0].ReferenceMax != 25 {
		t.Fatal("urine values changed")
	}
	for _, result := range results {
		if result.Review.Summary == "" {
			t.Fatal("missing per-study review")
		}
	}
}

func TestInvalidStudyRejectsEntireDocument(t *testing.T) {
	service := New(config.Config{})
	for _, raw := range []string{
		`{"studies":[{"markers":[{"name":"WBC","value":6.7}]},{}]}`,
		`{"studies":[{"markers":[{"name":"WBC","value":6.7},{"name":"Микроальбумин","value":0.6}]}]}`,
		`{"studies":[{"studies":[{}]}]}`,
	} {
		var extracted visionExtraction
		if err := json.Unmarshal([]byte(raw), &extracted); err != nil {
			t.Fatal(err)
		}
		results, err := service.finishVisionStudies(context.Background(), extracted, nil)
		if err == nil || results != nil {
			t.Fatalf("partial or mixed result accepted: %s", raw)
		}
	}
}
