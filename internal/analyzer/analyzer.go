package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
)

type Service struct {
	cfg    config.Config
	client *http.Client
}

func New(cfg config.Config) *Service {
	return &Service{cfg: cfg, client: &http.Client{Timeout: 60 * time.Second}}
}

func (s *Service) Process(ctx context.Context, path, mime string) (string, []domain.Marker, domain.AIReview, string) {
	text, err := s.extract(ctx, path, mime)
	if err != nil {
		return "", []domain.Marker{}, failedReview(), "failed"
	}
	markers := parseMarkers(text)
	review := ruleReview(markers)
	if s.cfg.DeepSeekAPIKey != "" && strings.TrimSpace(text) != "" {
		if m, r, e := s.deepSeek(ctx, text); e == nil {
			markers = m
			review = r
		}
	}
	status := "ready"
	if len(markers) == 0 {
		status = "needs_review"
		review = emptyReview()
	}
	return text, markers, review, status
}
func (s *Service) extract(ctx context.Context, path, mime string) (string, error) {
	if strings.Contains(mime, "pdf") {
		b, e := exec.CommandContext(ctx, "pdftotext", "-layout", path, "-").Output()
		if e == nil && len(bytes.TrimSpace(b)) > 20 {
			return string(b), nil
		}
		tmpDir, e := os.MkdirTemp("", "lab-pdf-ocr-")
		if e != nil {
			return "", e
		}
		defer os.RemoveAll(tmpDir)
		prefix := filepath.Join(tmpDir, "page")
		// Ограничение в 10 страниц защищает API от чрезмерно тяжёлых PDF.
		cmd := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", "200", "-f", "1", "-l", "10", path, prefix)
		if output, renderErr := cmd.CombinedOutput(); renderErr != nil {
			return "", fmt.Errorf("render pdf: %v: %s", renderErr, output)
		}
		pages, e := filepath.Glob(prefix + "-*.png")
		if e != nil || len(pages) == 0 {
			return "", fmt.Errorf("render pdf: no pages produced")
		}
		var text strings.Builder
		for _, page := range pages {
			b, ocrErr := exec.CommandContext(ctx, "tesseract", page, "stdout", "-l", s.cfg.TesseractLang).CombinedOutput()
			if ocrErr != nil {
				return "", fmt.Errorf("tesseract pdf page: %v: %s", ocrErr, b)
			}
			text.Write(b)
			text.WriteString("\n")
		}
		return text.String(), nil
	}
	cmd := exec.CommandContext(ctx, "tesseract", path, "stdout", "-l", s.cfg.TesseractLang)
	b, e := cmd.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("tesseract: %v: %s", e, b)
	}
	return string(b), nil
}

var row = regexp.MustCompile(`(?m)^\s*([\p{L}][\p{L}\s\-()/]{2,}?)\s+([<>]?\s*\d+(?:[.,]\d+)?)\s*([%\p{L}/^\d]*)\s*(?:\s+([\d.,]+)\s*[-–]\s*([\d.,]+))?\s*$`)

func parseMarkers(text string) []domain.Marker {
	out := []domain.Marker{}
	for _, m := range row.FindAllStringSubmatch(text, -1) {
		raw := strings.ReplaceAll(strings.TrimSpace(strings.TrimLeft(m[2], "<> ")), ",", ".")
		v, e := strconv.ParseFloat(raw, 64)
		if e != nil {
			continue
		}
		mk := domain.Marker{Name: strings.TrimSpace(m[1]), CanonicalName: strings.ToLower(strings.TrimSpace(m[1])), Value: &v, Unit: strings.TrimSpace(m[3]), Status: domain.StatusUnknown}
		if m[4] != "" {
			a, _ := strconv.ParseFloat(strings.ReplaceAll(m[4], ",", "."), 64)
			b, _ := strconv.ParseFloat(strings.ReplaceAll(m[5], ",", "."), 64)
			mk.ReferenceMin = &a
			mk.ReferenceMax = &b
			mk.Status = domain.StatusNormal
			if v < a {
				mk.Status = domain.StatusLow
			}
			if v > b {
				mk.Status = domain.StatusHigh
			}
		}
		out = append(out, mk)
	}
	return out
}
func ruleReview(markers []domain.Marker) domain.AIReview {
	abnormal := 0
	for _, m := range markers {
		if m.Status == domain.StatusHigh || m.Status == domain.StatusLow {
			abnormal++
		}
	}
	summary := "Показатели распознаны. Значимых отклонений по указанным лабораторией диапазонам не найдено."
	need := false
	urg := "routine"
	if abnormal > 0 {
		summary = fmt.Sprintf("Найдено показателей вне референсного диапазона: %d. Результат требует интерпретации с учётом симптомов и анамнеза.", abnormal)
		need = true
	}
	return domain.AIReview{Summary: summary, Lifestyle: []string{"Сохраняйте обычный режим сна и физической активности, если врач не рекомендовал иное."}, Nutrition: []string{"Не меняйте рацион радикально только на основании одного анализа."}, DoctorNeeded: need, Urgency: urg, Disclaimer: "Автоматическая оценка не является диагнозом и не заменяет консультацию врача. При резком ухудшении самочувствия обратитесь за неотложной помощью.", Provider: "rules"}
}

func emptyReview() domain.AIReview {
	return domain.AIReview{Summary: "Текст документа считан, но отдельные показатели автоматически выделить не удалось. Откройте оригинал и попробуйте загрузить более чёткое фото.", Lifestyle: []string{}, Nutrition: []string{}, DoctorNeeded: false, Urgency: "routine", Disclaimer: "Автоматическая обработка не является диагнозом и не заменяет консультацию врача.", Provider: "rules"}
}

func failedReview() domain.AIReview {
	return domain.AIReview{Summary: "Не удалось распознать документ. Оригинал сохранён — попробуйте загрузить более чёткое фото или PDF.", Lifestyle: []string{}, Nutrition: []string{}, DoctorNeeded: false, Urgency: "routine", Disclaimer: "Автоматическая обработка не является диагнозом и не заменяет консультацию врача.", Provider: "rules"}
}
func (s *Service) deepSeek(ctx context.Context, text string) ([]domain.Marker, domain.AIReview, error) {
	type msg struct{ Role, Content string }
	payload := map[string]any{
		"model":           s.cfg.DeepSeekModel,
		"temperature":     0.1,
		"max_tokens":      4096,
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"messages":        []msg{{"system", "Ты медицинский модуль структурирования лабораторных бланков. Верни только json-объект: markers (поля name, canonical_name, value, text_value, unit, reference_min, reference_max, reference_text, status low|normal|high|unknown) и ai_review (summary, lifestyle[], nutrition[], doctor_needed, urgency routine|soon|urgent, suggested_specialty). Не ставь диагноз. Не выдумывай отсутствующие значения."}, {"user", text}},
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.DeepSeekBaseURL, "/")+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.cfg.DeepSeekAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, e := s.client.Do(req)
	if e != nil {
		return nil, domain.AIReview{}, e
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, domain.AIReview{}, fmt.Errorf("deepseek status %d", resp.StatusCode)
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&envelope); e != nil || len(envelope.Choices) == 0 {
		return nil, domain.AIReview{}, fmt.Errorf("invalid deepseek response")
	}
	var out struct {
		Markers  []domain.Marker `json:"markers"`
		AIReview domain.AIReview `json:"ai_review"`
	}
	if e = json.Unmarshal([]byte(envelope.Choices[0].Message.Content), &out); e != nil {
		return nil, domain.AIReview{}, e
	}
	out.AIReview.Provider = "deepseek"
	out.AIReview.Disclaimer = "Автоматическая оценка не является диагнозом и не заменяет консультацию врача. При резком ухудшении самочувствия обратитесь за неотложной помощью."
	return out.Markers, out.AIReview, nil
}
