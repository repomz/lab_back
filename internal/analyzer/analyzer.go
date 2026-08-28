package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
		if m, r, e := s.deepSeek(ctx, text); e != nil {
			log.Printf("deepseek structuring failed: %v", e)
		} else if len(m) > 0 {
			markers = m
			review = r
		} else {
			log.Printf("deepseek structuring returned no markers")
		}
	}
	status := "ready"
	if len(markers) == 0 {
		status = "needs_review"
		review = emptyReview()
	} else if markersNeedReview(markers) {
		status = "needs_review"
		review.Summary += " Часть полей распознана неуверенно — сверьте их с оригиналом."
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
			pageText, ocrErr := s.extractImage(ctx, page)
			if ocrErr != nil {
				return "", fmt.Errorf("tesseract pdf page: %w", ocrErr)
			}
			text.WriteString(pageText)
			text.WriteString("\n")
		}
		return text.String(), nil
	}
	return s.extractImage(ctx, path)
}

func (s *Service) extractImage(ctx context.Context, path string) (string, error) {
	ocrPath, cleanup, preprocessErr := preprocessImage(ctx, path)
	if cleanup != nil {
		defer cleanup()
	}
	if preprocessErr != nil {
		log.Printf("image preprocessing failed, using original: %v", preprocessErr)
		ocrPath = path
	}
	var candidates []string
	var lastErr error
	// PSM 4 лучше видит колонки, PSM 11 — разреженный текст
	// на фотографиях с полями и печатями.
	for _, psm := range []string{"4", "11"} {
		cmd := exec.CommandContext(ctx, "tesseract", ocrPath, "stdout", "-l", s.cfg.TesseractLang, "--psm", psm, "-c", "preserve_interword_spaces=1")
		b, err := cmd.CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("psm %s: %v: %s", psm, err, b)
			continue
		}
		if strings.TrimSpace(string(b)) != "" {
			candidates = append(candidates, string(b))
		}
	}
	if len(candidates) == 0 {
		if lastErr != nil {
			return "", lastErr
		}
		return "", fmt.Errorf("tesseract returned empty text")
	}
	best := candidates[0]
	bestScore := ocrScore(best)
	for _, candidate := range candidates[1:] {
		if score := ocrScore(candidate); score > bestScore {
			best = candidate
			bestScore = score
		}
	}
	return best, nil
}

// preprocessImage fixes the EXIF orientation commonly produced by phone cameras,
// normalizes uneven lighting and removes a small camera tilt before OCR. Tesseract
// does not reliably apply EXIF orientation itself.
func preprocessImage(ctx context.Context, path string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "lab-image-ocr-")
	if err != nil {
		return path, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	outputPath := filepath.Join(tmpDir, "normalized.png")
	// OCR does not benefit from 12+ MP phone photos, while processing time grows
	// roughly with pixel count. 1800 px keeps small lab-table text readable and
	// prevents a single upload from occupying the small production CPU too long.
	cmd := exec.CommandContext(ctx, "magick", path, "-auto-orient", "-resize", "1800x1800>", "-colorspace", "Gray", "-deskew", "40%", "-contrast-stretch", "1%x1%", "-sharpen", "0x1", outputPath)
	if output, commandErr := cmd.CombinedOutput(); commandErr != nil {
		cleanup()
		return path, nil, fmt.Errorf("magick: %v: %s", commandErr, output)
	}
	return outputPath, cleanup, nil
}

func ocrScore(text string) int {
	decimalValues := len(regexp.MustCompile(`\d+[.,]\d+`).FindAllString(text, -1))
	lower := strings.ToLower(text)
	labTerms := 0
	for _, term := range []string{"глюкоз", "альбумин", "билирубин", "креатинин", "мочевин", "холестерин"} {
		if strings.Contains(lower, term) {
			labTerms++
		}
	}
	return len(parseMarkers(text))*10000 + decimalValues*200 + labTerms*100 + len(strings.Fields(text))
}

type markerSpec struct {
	name, canonical, unit string
	aliases               []string
	maxPlausible          float64
}

var markerSpecs = []markerSpec{
	{"Глюкоза", "glucose", "ммоль/л", []string{"глюкоз"}, 100},
	{"Альбумин", "albumin", "г/л", []string{"альбумин"}, 100},
	{"Билирубин общий", "bilirubin_total", "мкмоль/л", []string{"билирубин общий"}, 1000},
	{"Билирубин прямой", "bilirubin_direct", "мкмоль/л", []string{"билирубин прямой"}, 500},
	{"АЛТ", "alt", "Ед/л", []string{"алт"}, 10000},
	{"АСТ", "ast", "Ед/л", []string{"аст", "act"}, 10000},
	{"Холестерин общий", "cholesterol_total", "ммоль/л", []string{"холестерин общий"}, 100},
	{"Триглицериды", "triglycerides", "ммоль/л", []string{"триглицерид"}, 100},
	{"ЛПВП", "hdl", "ммоль/л", []string{"лпвп", "nnen"}, 20},
	{"ЛПНП", "ldl", "ммоль/л", []string{"лпнп", "аннп", "anhn"}, 30},
	{"Мочевина", "urea", "ммоль/л", []string{"мочевин"}, 100},
	{"Креатинин", "creatinine", "мкмоль/л", []string{"креатинин"}, 2000},
	{"СКФ", "egfr", "мл/мин/1,73 м²", []string{"фильтрац"}, 200},
	{"СРБ", "crp", "мг/л", []string{"срб", "cpb"}, 1000},
	{"Мочевая кислота", "uric_acid", "мкмоль/л", []string{"мочевая кислота", "mouebas кислота"}, 2000},
	{"Железо", "iron", "мкмоль/л", []string{"железо"}, 200},
	{"Кальций общий", "calcium_total", "ммоль/л", []string{"кальций"}, 10},
	{"Калий", "potassium", "ммоль/л", []string{"калий"}, 20},
	{"Натрий", "sodium", "ммоль/л", []string{"натрий"}, 200},
}

var (
	row              = regexp.MustCompile(`(?m)^[ \t]*([\p{L}][\p{L} \t\-()/]{2,}?)[ \t]+([<>]?[ \t]*\d+(?:[.,]\d+)?)[ \t]*([%\p{L}/^\d]*)[ \t]*(?:[ \t]+([\d.,]+)[ \t]*[-–][ \t]*([\d.,]+))?[ \t]*$`)
	numberToken      = regexp.MustCompile(`(?:>=|<=|>|<)?[ \t]*\d+(?:[.,]\d+)?`)
	rangeToken       = regexp.MustCompile(`(\d+(?:[.,]\d+)?)[ \t]*[-–=][ \t]*(\d+(?:[.,]\d+)?)`)
	thresholdToken   = regexp.MustCompile(`(>=|<=|>|<)[ \t]*(\d+(?:[.,]\d+)?)`)
	leadingParenCode = regexp.MustCompile(`^[ \t]*\([^)]*\)`)
)

func parseMarkers(text string) []domain.Marker {
	known := parseKnownMarkers(text)
	if len(known) >= 3 {
		return known
	}

	out := append([]domain.Marker{}, known...)
	seen := map[string]bool{}
	for _, marker := range known {
		seen[marker.CanonicalName] = true
	}
	for _, m := range row.FindAllStringSubmatch(text, -1) {
		raw := strings.ReplaceAll(strings.TrimSpace(strings.TrimLeft(m[2], "<> ")), ",", ".")
		v, e := strconv.ParseFloat(raw, 64)
		if e != nil {
			continue
		}
		name := strings.TrimSpace(m[1])
		canonical := strings.ToLower(name)
		if seen[canonical] || isKnownMarkerLabel(canonical) || (strings.TrimSpace(m[3]) == "" && m[4] == "") || isAdministrativeLabel(canonical) {
			continue
		}
		mk := domain.Marker{Name: name, CanonicalName: canonical, Value: &v, Unit: strings.TrimSpace(m[3]), Status: domain.StatusUnknown}
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
		seen[canonical] = true
	}
	return out
}

func parseKnownMarkers(text string) []domain.Marker {
	found := map[string]domain.Marker{}
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		lower := strings.ToLower(strings.ReplaceAll(line, "ё", "е"))
		for _, spec := range markerSpecs {
			if _, exists := found[spec.canonical]; exists {
				continue
			}
			alias, index := matchedAlias(lower, spec.aliases)
			if index < 0 {
				continue
			}
			after := leadingParenCode.ReplaceAllString(lower[index+len(alias):], "")
			numbers := numberToken.FindAllString(after, -1)
			if len(numbers) == 0 {
				continue
			}
			value, ok := parseOCRNumber(numbers[0])
			if !ok {
				continue
			}
			value = normalizeMarkerValue(value, numbers[0], spec.maxPlausible)
			marker := domain.Marker{Name: spec.name, CanonicalName: spec.canonical, Value: floatPtr(value), Unit: spec.unit, Status: domain.StatusUnknown}
			prefix := lower[:index]
			hint := domain.StatusUnknown
			if strings.Contains(prefix, ">") {
				hint = domain.StatusHigh
			} else if strings.Contains(prefix, "<") {
				hint = domain.StatusLow
			}
			if match := rangeToken.FindStringSubmatch(after); len(match) == 3 {
				a, okA := parseOCRNumber(match[1])
				b, okB := parseOCRNumber(match[2])
				if okA && okB {
					a, b = normalizeReferenceRange(value, a, b, hint)
					marker.ReferenceMin, marker.ReferenceMax = floatPtr(a), floatPtr(b)
					marker.ReferenceText = formatReference(a, b)
					marker.Status = statusForRange(value, a, b)
				}
			} else if match := thresholdToken.FindStringSubmatch(after); len(match) == 3 {
				threshold, okThreshold := parseOCRNumber(match[2])
				if okThreshold {
					marker.ReferenceText = match[1] + " " + formatNumber(threshold)
					switch match[1] {
					case ">", ">=":
						marker.ReferenceMin = floatPtr(threshold)
						if value < threshold {
							marker.Status = domain.StatusLow
						} else {
							marker.Status = domain.StatusNormal
						}
					case "<", "<=":
						marker.ReferenceMax = floatPtr(threshold)
						if value > threshold {
							marker.Status = domain.StatusHigh
						} else {
							marker.Status = domain.StatusNormal
						}
					}
				}
			}
			if hint != domain.StatusUnknown {
				marker.Status = hint
			}
			found[spec.canonical] = marker
		}
	}
	out := make([]domain.Marker, 0, len(found))
	for _, spec := range markerSpecs {
		if marker, ok := found[spec.canonical]; ok {
			out = append(out, marker)
		}
	}
	return out
}

func matchedAlias(line string, aliases []string) (string, int) {
	for _, alias := range aliases {
		if index := strings.Index(line, alias); index >= 0 {
			return alias, index
		}
	}
	return "", -1
}

func parseOCRNumber(raw string) (float64, bool) {
	clean := strings.NewReplacer(">=", "", "<=", "", ">", "", "<", "", " ", "", ",", ".").Replace(raw)
	value, err := strconv.ParseFloat(clean, 64)
	return value, err == nil
}

func normalizeMarkerValue(value float64, raw string, maxPlausible float64) float64 {
	if strings.ContainsAny(raw, ",.") || maxPlausible <= 0 {
		return value
	}
	for value > maxPlausible {
		value /= 10
	}
	return value
}

func normalizeReferenceRange(value, rawMin, rawMax float64, hint domain.MarkerStatus) (float64, float64) {
	if value == 0 {
		return rawMin, rawMax
	}
	bestMin, bestMax := rawMin, rawMax
	bestScore := 1.7976931348623157e+308
	for minScale, minDivisor := 0, 1.0; minScale <= 3; minScale, minDivisor = minScale+1, minDivisor*10 {
		for maxScale, maxDivisor := 0, 1.0; maxScale <= 3; maxScale, maxDivisor = maxScale+1, maxDivisor*10 {
			minValue, maxValue := rawMin/minDivisor, rawMax/maxDivisor
			if minValue > maxValue {
				continue
			}
			valid := minValue <= value && value <= maxValue
			if hint == domain.StatusHigh {
				valid = minValue <= value && value > maxValue
			} else if hint == domain.StatusLow {
				valid = value < minValue && value <= maxValue
			}
			score := maxValue - minValue
			if hint == domain.StatusHigh {
				score += (value - maxValue) * 1000
			} else if hint == domain.StatusLow {
				score += (minValue - value) * 1000
			}
			if valid && score < bestScore {
				bestMin, bestMax, bestScore = minValue, maxValue, score
			}
		}
	}
	return bestMin, bestMax
}

func statusForRange(value, minValue, maxValue float64) domain.MarkerStatus {
	if value < minValue {
		return domain.StatusLow
	}
	if value > maxValue {
		return domain.StatusHigh
	}
	return domain.StatusNormal
}

func formatReference(minValue, maxValue float64) string {
	return formatNumber(minValue) + " - " + formatNumber(maxValue)
}

func formatNumber(value float64) string {
	return strings.ReplaceAll(strconv.FormatFloat(value, 'f', -1, 64), ".", ",")
}

func floatPtr(value float64) *float64 { return &value }

func isAdministrativeLabel(name string) bool {
	for _, label := range []string{"окпо", "инн", "кпп", "фио", "полис", "образца", "карты", "тел"} {
		if strings.Contains(name, label) {
			return true
		}
	}
	return false
}

func isKnownMarkerLabel(name string) bool {
	for _, spec := range markerSpecs {
		if _, index := matchedAlias(strings.ToLower(strings.ReplaceAll(name, "ё", "е")), spec.aliases); index >= 0 {
			return true
		}
	}
	return false
}

func markersNeedReview(markers []domain.Marker) bool {
	for _, marker := range markers {
		if marker.Value == nil || marker.Status == domain.StatusUnknown || marker.Unit == "" {
			return true
		}
	}
	return false
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
