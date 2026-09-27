package analyzer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/repomz/lab_back/internal/domain"
)

// DocumentResult is a complete, synchronous interpretation of one uploaded
// document. The HTTP handler persists it only after every stage has completed.
type DocumentResult struct {
	MedicalText string
	Title       string
	Category    string
	CollectedAt *time.Time
	Markers     []domain.Marker
	Report      *domain.StudyReport
	Review      domain.AIReview
}

type visionExtraction struct {
	Studies      []visionExtraction  `json:"studies,omitempty"`
	DocumentType string              `json:"document_type"`
	Title        string              `json:"title"`
	Category     string              `json:"category"`
	CollectedAt  string              `json:"collected_at"`
	MedicalText  string              `json:"medical_text"`
	Markers      []visionMarker      `json:"markers"`
	Report       *domain.StudyReport `json:"report"`
}

type visionMarker struct {
	Name          string          `json:"name"`
	CanonicalName string          `json:"canonical_name"`
	Value         json.RawMessage `json:"value"`
	TextValue     string          `json:"text_value"`
	Unit          string          `json:"unit"`
	ReferenceMin  *float64        `json:"reference_min"`
	ReferenceMax  *float64        `json:"reference_max"`
	ReferenceText string          `json:"reference_text"`
	Status        string          `json:"status"`
}

type visionImage struct {
	MIME string
	Data []byte
}

// AnalyzeDocumentForPatient sends the original pages to the multimodal model,
// validates the returned structure, and creates the patient-facing review in
// one uninterrupted request. It intentionally does not fall back to publishing
// uncertain Tesseract output as a successful result.
func (s *Service) AnalyzeDocumentForPatient(ctx context.Context, path, mime string, profile *domain.PatientProfile) (DocumentResult, error) {
	results, err := s.AnalyzeDocumentsForPatient(ctx, path, mime, profile)
	if err != nil {
		return DocumentResult{}, err
	}
	if len(results) != 1 {
		return DocumentResult{}, fmt.Errorf("document contains %d separate studies", len(results))
	}
	return results[0], nil
}

func (s *Service) AnalyzeDocumentsForPatient(ctx context.Context, path, mime string, profile *domain.PatientProfile) ([]DocumentResult, error) {
	if strings.TrimSpace(s.cfg.DeepSeekAPIKey) == "" {
		return nil, fmt.Errorf("visual recognition is not configured")
	}
	images, cleanup, err := prepareVisionImages(ctx, path, mime)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}

	content := make([]map[string]any, 0, len(images)+1)
	content = append(content, map[string]any{"type": "text", "text": visionExtractionPrompt})
	for _, image := range images {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:" + image.MIME + ";base64," + base64.StdEncoding.EncodeToString(image.Data)},
		})
	}
	model := strings.TrimSpace(s.cfg.DeepSeekVisionModel)
	if model == "" {
		model = "deepseek-flash"
	}
	payload := map[string]any{
		"model": model, "temperature": 0, "max_tokens": 8192,
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]any{
			{"role": "system", "content": "Ты модуль точной визуальной транскрипции медицинских документов. Копируй только видимые данные, не дополняй их типовыми нормами или медицинскими знаниями. Верни только JSON."},
			{"role": "user", "content": content},
		},
	}
	response, err := s.requestDeepSeekVision(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("visual recognition: %w", err)
	}
	var extracted visionExtraction
	if err = json.Unmarshal([]byte(response), &extracted); err != nil {
		return nil, fmt.Errorf("invalid visual recognition response: %w", err)
	}
	// A second visual pass compares the draft against the original pixels. It
	// catches column shifts and easily confused glyphs (I/1, decimal commas,
	// abbreviations) without using generic medical ranges as a substitute.
	verified, verifyErr := s.verifyVisionExtraction(ctx, images, extracted)
	if verifyErr != nil {
		return nil, fmt.Errorf("visual verification: %w", verifyErr)
	}
	return s.finishVisionStudies(ctx, verified, profile)
}

func (s *Service) finishVisionStudies(ctx context.Context, extracted visionExtraction, profile *domain.PatientProfile) ([]DocumentResult, error) {
	studies := extracted.Studies
	if len(studies) == 0 {
		studies = []visionExtraction{extracted}
	}
	if len(studies) > 20 {
		return nil, fmt.Errorf("too many studies in document")
	}
	results := make([]DocumentResult, 0, len(studies))
	for _, study := range studies {
		if len(study.Studies) > 0 {
			return nil, fmt.Errorf("nested study groups are invalid")
		}
		if len(study.Markers) > 0 && study.Report != nil {
			return nil, fmt.Errorf("laboratory and diagnostic studies must be separate")
		}
		// Do not silently publish the known cross-specimen failure even if both
		// model passes repeat it. Ask for another attempt instead of mislabelling.
		hasBlood, hasMicroalbumin := false, false
		for _, marker := range study.Markers {
			key := normalizeSearch(marker.CanonicalName + " " + marker.Name)
			if containsAny(key, "microalbumin", "микроальбумин") {
				hasMicroalbumin = true
			}
			if containsAny(key, "wbc", "rbc", "hgb", "plt", "гемоглобин", "тромбоцит") {
				hasBlood = true
			}
		}
		if hasBlood && hasMicroalbumin {
			return nil, fmt.Errorf("blood count and urine microalbumin must be separate studies")
		}
		result, err := s.finishVisionExtraction(ctx, study, profile)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Service) verifyVisionExtraction(ctx context.Context, images []visionImage, draft visionExtraction) (visionExtraction, error) {
	draftJSON, err := json.Marshal(draft)
	if err != nil {
		return visionExtraction{}, err
	}
	content := []map[string]any{{"type": "text", "text": `Повторно сверь черновик с оригинальными страницами посимвольно. Проверь границы исследований: разные бланки, биоматериалы, даты или протоколы должны быть отдельными элементами studies; продолжения одного бланка объедини, не дублируй. ОАК и микроальбумин мочи обязательно раздели. Исправь только реальные расхождения с изображением: пропущенные строки, перепутанные колонки, цифры, десятичные знаки, I/1, сокращения, единицы, референсы, описание и заключение. Не исправляй опечатки самого исходного документа, не добавляй типовые нормы и не делай медицинских выводов. Верни полный исправленный JSON в той же схеме. Черновик: ` + string(draftJSON)}}
	for _, image := range images {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:" + image.MIME + ";base64," + base64.StdEncoding.EncodeToString(image.Data)},
		})
	}
	model := strings.TrimSpace(s.cfg.DeepSeekVisionModel)
	if model == "" {
		model = "deepseek-flash"
	}
	payload := map[string]any{
		"model": model, "temperature": 0, "max_tokens": 8192,
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]any{
			{"role": "system", "content": "Ты независимый контролёр точности визуальной транскрипции медицинского документа. Единственный источник истины — изображение."},
			{"role": "user", "content": content},
		},
	}
	response, err := s.requestDeepSeekVision(ctx, payload)
	if err != nil {
		return visionExtraction{}, err
	}
	var verified visionExtraction
	if err = json.Unmarshal([]byte(response), &verified); err != nil {
		return visionExtraction{}, err
	}
	return verified, nil
}

const visionExtractionPrompt = `Прочитай все приложенные страницы и выдели каждое самостоятельное исследование.
Одна фотография или PDF может содержать 2, 3 и более разных исследований. Верни их отдельными элементами studies с собственными датой, показателями, описанием, заключением и названием.
Разделяй по заголовкам бланков, виду исследования, биоматериалу, дате и номеру образца. ОАК с лейкоформулой и СОЭ одного образца оставь вместе; микроальбумин мочи на том же листе — отдельное исследование. УЗИ разных органов с самостоятельными протоколами разделяй. Продолжение одного бланка на следующей странице объедини. Не дублируй строки и не дроби один профиль на отдельные показатели. Не смешивай кровь и мочу.
Требования к точности:
1. Не угадывай и не исправляй значения по медицинским знаниям. Используй только то, что видно на бланке.
2. Для лабораторной таблицы верни каждую строку исследования. Результат, единицу и референс бери только из той же строки/колонки. Если референс на бланке пуст, все reference-поля должны быть null/пустыми.
3. Десятичные запятые передавай JSON-числом с точкой. Диапазон результата (например 1-2 или 6-8), знаки +/++++, «отрицательно», «норма» передавай целиком в text_value, а value=null.
4. reference_min/reference_max заполняй только для явно напечатанных числовых границ. Исходный вид диапазона всегда копируй в reference_text. Односторонний диапазон заполняй одной границей.
5. status определяй только по напечатанному референсу или флагу лаборатории: low, normal, high, unknown. Без референса — unknown.
6. Для УЗИ, КТ, МРТ или рентгена дословно перепиши медицинское описание в report.description и формальное заключение в report.conclusion. Не превращай описание в собственный диагноз. Для лабораторного документа report=null.
7. medical_text должен содержать аккуратную транскрипцию только медицинской части документа без ФИО, адреса, полиса, номера карты и иных идентификаторов пациента.
8. collected_at — дата забора материала или дата исследования в YYYY-MM-DD; не дата рождения, печати или направления. Если её нет, пустая строка.
9. title — короткое понятное название вида исследования, без перечисления всех строк бланка. category не является свободным текстом: выбери одну рубрику из «Кровь», «Моча», «УЗИ», «КТ и МРТ», «Рентген», «Другие анализы».
Верни JSON {"studies":[...]} даже для одного исследования. Каждый элемент массива строго такой формы:
{"document_type":"laboratory|diagnostic","title":"краткое название исследования","category":"категория","collected_at":"YYYY-MM-DD или пусто","medical_text":"транскрипция медицинской части","markers":[{"name":"как на бланке","canonical_name":"стабильное английское имя или транслитерация","value":null,"text_value":"","unit":"","reference_min":null,"reference_max":null,"reference_text":"","status":"low|normal|high|unknown"}],"report":null}
Для диагностического исследования markers=[] и report={"modality":"УЗИ|КТ|МРТ|Рентген","study_name":"название","description":"дословное описание","conclusion":"дословное заключение","confidence":1,"warnings":[]}.`

func (s *Service) finishVisionExtraction(ctx context.Context, extracted visionExtraction, profile *domain.PatientProfile) (DocumentResult, error) {
	result := DocumentResult{
		MedicalText: trimRunes(sanitizeMedicalText(extracted.MedicalText), 50000),
		Title:       trimRunes(strings.TrimSpace(extracted.Title), 240),
		Category:    trimRunes(strings.TrimSpace(extracted.Category), 160),
		CollectedAt: parseVisionDate(extracted.CollectedAt),
		Markers:     []domain.Marker{},
	}
	for _, raw := range extracted.Markers {
		marker, err := raw.domainMarker()
		if err != nil {
			return DocumentResult{}, err
		}
		result.Markers = append(result.Markers, marker)
	}
	if len(result.Markers) > 0 {
		result.Markers, _ = NormalizeConfirmedMarkers(result.Markers)
		if len(result.Markers) == 0 {
			return DocumentResult{}, fmt.Errorf("visual recognition returned no valid markers")
		}
		for i := range result.Markers {
			marker := &result.Markers[i]
			if marker.ReferenceMin == nil && marker.ReferenceMax == nil && strings.TrimSpace(marker.ReferenceText) == "" {
				// A positive/negative result is still just the printed value when
				// the laboratory did not print a reference beside it.
				marker.Status = domain.StatusUnknown
			}
		}
		result.Review = s.ReviewMarkersForPatient(ctx, result.Markers, profile)
	} else if extracted.Report != nil {
		candidate := *extracted.Report
		candidate.Description = sanitizeMedicalText(candidate.Description)
		candidate.Conclusion = sanitizeMedicalText(candidate.Conclusion)
		report, err := NormalizeConfirmedReport(&candidate)
		if err != nil {
			return DocumentResult{}, fmt.Errorf("invalid diagnostic report: %w", err)
		}
		result.Report = report
		result.Review = s.ReviewStudyReportForPatient(ctx, report, profile)
	} else {
		return DocumentResult{}, fmt.Errorf("visual recognition found neither laboratory values nor a diagnostic report")
	}
	// Provider labels are suggestions only. One deterministic taxonomy keeps
	// new and historical documents in the same patient-facing rubric.
	result.Title, result.Category = CanonicalAnalysisIdentity(result.Markers, result.MedicalText, result.Report)
	return result, nil
}

func sanitizeMedicalText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r", ""), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(strings.ReplaceAll(trimmed, "ё", "е"))
		if trimmed == "" {
			continue
		}
		// The visual provider needs the original pixels for recognition, but
		// administrative identifiers are not retained in MongoDB afterwards.
		if strings.Contains(lower, "фио пациент") || strings.Contains(lower, "фамилия, имя") ||
			strings.Contains(lower, "дата рождения") || strings.Contains(lower, "медицинск") && strings.Contains(lower, "полис") ||
			strings.Contains(lower, "номер медицинской карты") || strings.Contains(lower, "№ амбулаторной карты") ||
			strings.HasPrefix(lower, "адрес:") {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func (m visionMarker) domainMarker() (domain.Marker, error) {
	marker := domain.Marker{
		Name: strings.TrimSpace(m.Name), CanonicalName: strings.TrimSpace(m.CanonicalName), TextValue: strings.TrimSpace(m.TextValue),
		Unit: strings.TrimSpace(m.Unit), ReferenceMin: m.ReferenceMin, ReferenceMax: m.ReferenceMax,
		ReferenceText: strings.TrimSpace(m.ReferenceText), Status: domain.MarkerStatus(strings.ToLower(strings.TrimSpace(m.Status))), Confidence: 1,
	}
	if marker.Name == "" {
		return domain.Marker{}, fmt.Errorf("visual recognition returned an unnamed marker")
	}
	if marker.CanonicalName == "" {
		marker.CanonicalName = strings.ToLower(strings.ReplaceAll(marker.Name, " ", "_"))
	}
	raw := strings.TrimSpace(string(m.Value))
	if raw != "" && raw != "null" {
		var value float64
		if err := json.Unmarshal(m.Value, &value); err != nil {
			var text string
			if json.Unmarshal(m.Value, &text) != nil {
				return domain.Marker{}, fmt.Errorf("invalid value for %s", marker.Name)
			}
			text = strings.ReplaceAll(strings.TrimSpace(text), ",", ".")
			value, err = strconv.ParseFloat(text, 64)
			if err != nil {
				if marker.TextValue == "" {
					marker.TextValue = text
				}
				return marker, nil
			}
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return domain.Marker{}, fmt.Errorf("invalid value for %s", marker.Name)
		}
		marker.Value = &value
	}
	return marker, nil
}

func prepareVisionImages(ctx context.Context, path, mime string) ([]visionImage, func(), error) {
	if strings.Contains(strings.ToLower(mime), "pdf") {
		info, err := exec.CommandContext(ctx, "pdfinfo", path).Output()
		if err != nil {
			return nil, nil, fmt.Errorf("cannot read PDF page count: %w", err)
		}
		pageCount := 0
		for _, line := range strings.Split(string(info), "\n") {
			if strings.HasPrefix(line, "Pages:") {
				pageCount, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			}
		}
		if pageCount < 1 || pageCount > 10 {
			return nil, nil, fmt.Errorf("PDF must contain 1–10 pages; split longer documents before uploading")
		}
		dir, err := os.MkdirTemp("", "lab-vision-pdf-")
		if err != nil {
			return nil, nil, err
		}
		cleanup := func() { _ = os.RemoveAll(dir) }
		prefix := filepath.Join(dir, "page")
		if output, renderErr := exec.CommandContext(ctx, "pdftoppm", "-jpeg", "-r", "180", "-f", "1", "-l", strconv.Itoa(pageCount), path, prefix).CombinedOutput(); renderErr != nil {
			cleanup()
			return nil, nil, fmt.Errorf("render pdf: %v: %s", renderErr, output)
		}
		pages, _ := filepath.Glob(prefix + "-*.jpg")
		if len(pages) != pageCount {
			cleanup()
			return nil, nil, fmt.Errorf("render pdf: expected %d pages, got %d", pageCount, len(pages))
		}
		images := make([]visionImage, 0, len(pages))
		for _, page := range pages {
			data, readErr := os.ReadFile(page)
			if readErr != nil {
				cleanup()
				return nil, nil, readErr
			}
			images = append(images, visionImage{MIME: "image/jpeg", Data: data})
		}
		return images, cleanup, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	detected := http.DetectContentType(data)
	if strings.HasPrefix(detected, "image/jpeg") || strings.HasPrefix(detected, "image/png") || strings.HasPrefix(detected, "image/gif") || strings.HasPrefix(detected, "image/webp") {
		return []visionImage{{MIME: strings.Split(detected, ";")[0], Data: data}}, nil, nil
	}
	dir, err := os.MkdirTemp("", "lab-vision-image-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	converted := filepath.Join(dir, "document.jpg")
	if output, convertErr := exec.CommandContext(ctx, "convert", path+"[0]", "-auto-orient", "-quality", "92", converted).CombinedOutput(); convertErr != nil {
		cleanup()
		return nil, nil, fmt.Errorf("convert image: %v: %s", convertErr, output)
	}
	data, err = os.ReadFile(converted)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return []visionImage{{MIME: "image/jpeg", Data: data}}, cleanup, nil
}

func parseVisionDate(value string) *time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02", "02.01.2006", "2.1.2006"} {
		if parsed, err := time.Parse(layout, value); err == nil && parsed.Year() >= 1900 && !parsed.After(time.Now().Add(24*time.Hour)) {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func trimRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max])
	}
	return value
}
