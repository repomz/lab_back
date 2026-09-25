package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
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
	cfg       config.Config
	client    *http.Client
	ocrSlots  chan struct{}
	aiLimiter *deepSeekLimiter
}

func New(cfg config.Config) *Service {
	// OCR concurrency follows the durable worker count. This keeps a small host
	// stable and lets a larger deployment scale by configuration.
	if cfg.OCRWorkerCount <= 0 {
		cfg.OCRWorkerCount = 1
	}
	if cfg.DeepSeekRequestsPerMinute <= 0 {
		cfg.DeepSeekRequestsPerMinute = defaultDeepSeekRequestsPerMinute
	}
	if cfg.DeepSeekRequestsPerHour <= 0 {
		cfg.DeepSeekRequestsPerHour = defaultDeepSeekRequestsPerHour
	}
	if cfg.DeepSeekMaxConcurrent <= 0 {
		cfg.DeepSeekMaxConcurrent = defaultDeepSeekMaxConcurrent
	}
	if cfg.DeepSeekTimeoutSeconds <= 0 {
		cfg.DeepSeekTimeoutSeconds = int(defaultDeepSeekTimeout / time.Second)
	}
	return &Service{
		cfg:       cfg,
		client:    &http.Client{Timeout: time.Duration(cfg.DeepSeekTimeoutSeconds+5) * time.Second},
		ocrSlots:  make(chan struct{}, cfg.OCRWorkerCount),
		aiLimiter: newDeepSeekLimiter(cfg.DeepSeekRequestsPerMinute, cfg.DeepSeekRequestsPerHour, cfg.DeepSeekMaxConcurrent),
	}
}

func (s *Service) Process(ctx context.Context, path, mime string) (string, []domain.Marker, domain.AIReview, string) {
	return s.ProcessForPatient(ctx, path, mime, nil)
}

func (s *Service) ProcessForPatient(ctx context.Context, path, mime string, profile *domain.PatientProfile) (string, []domain.Marker, domain.AIReview, string) {
	text, markers, status := s.RecognizeForPatient(ctx, path, mime, profile)
	if status == "failed" {
		return text, markers, failedReview(), status
	}
	if len(markers) == 0 {
		return text, markers, emptyReview(), status
	}
	review := s.ReviewMarkersForPatient(ctx, markers, profile)
	return text, markers, review, "ready"
}

// RecognizeForPatient extracts a verifiable table but deliberately postpones
// interpretation until the patient confirms the values.
func (s *Service) RecognizeForPatient(ctx context.Context, path, mime string, profile *domain.PatientProfile) (string, []domain.Marker, string) {
	text, markers, _, status, _ := s.RecognizeJob(ctx, path, mime, profile, nil)
	return text, markers, status
}

type RecognitionProgress = func(stage string, progress int)

// RecognizeJob is the durable-worker entry point. Unlike the compatibility
// wrapper above it returns transient failures so the queue can retry them.
func (s *Service) RecognizeJob(ctx context.Context, path, mime string, profile *domain.PatientProfile, progress RecognitionProgress) (string, []domain.Marker, *domain.StudyReport, string, error) {
	started := time.Now()
	report := func(stage string, value int) {
		if progress != nil {
			progress(stage, value)
		}
	}
	select {
	case s.ocrSlots <- struct{}{}:
	case <-ctx.Done():
		return "", []domain.Marker{}, nil, domain.AnalysisStatusFailed, ctx.Err()
	}
	defer func() { <-s.ocrSlots }()
	report(domain.ProcessingStagePreprocessing, 15)
	report(domain.ProcessingStageRecognizing, 35)
	text, candidates, err := s.extract(ctx, path, mime)
	if err != nil {
		log.Printf("recognition failed file=%s stage=ocr elapsed=%s error=%v", filepath.Base(path), time.Since(started).Round(time.Millisecond), err)
		return "", []domain.Marker{}, nil, domain.AnalysisStatusFailed, err
	}
	ocrFinished := time.Now()
	report(domain.ProcessingStageStructuring, 76)
	markers := parseOCRCandidates(candidates)
	reportData := ExtractStudyReport(text)
	// Raw OCR can contain names, policy numbers and other identifiers. It is
	// deliberately never sent to an external language model. DeepSeek receives
	// only the compact marker set after the patient has verified it.
	report(domain.ProcessingStageFinalizing, 94)
	status := domain.AnalysisStatusAwaitingConfirmation
	if len(markers) == 0 && reportData == nil {
		status = domain.AnalysisStatusNeedsReview
	}
	log.Printf("recognition complete file=%s ocr=%s total=%s markers=%d report=%t status=%s", filepath.Base(path), ocrFinished.Sub(started).Round(time.Millisecond), time.Since(started).Round(time.Millisecond), len(markers), reportData != nil, status)
	return text, markers, reportData, status, nil
}

func (s *Service) ReviewMarkersForPatient(ctx context.Context, markers []domain.Marker, profile *domain.PatientProfile) domain.AIReview {
	fallback := ruleReview(markers)
	if s.cfg.DeepSeekAPIKey == "" {
		return fallback
	}
	aiCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	review, err := s.deepSeekReview(aiCtx, markers, profile)
	if err != nil {
		log.Printf("deepseek review failed: %v", err)
		return fallback
	}
	return review
}

// A mostly complete table is faster and safer to show directly for patient
// verification. DeepSeek structures only genuinely sparse OCR here; medical
// interpretation still always runs after the patient approves the values.
func markersNeedStructuring(markers []domain.Marker) bool {
	if len(markers) < 8 {
		return true
	}
	// A long table is already useful for the mandatory patient verification
	// screen. Asking a language model to "repair" one uncertain reference adds
	// latency and can replace a laboratory-specific range with a generic one.
	if len(markers) >= 12 {
		return false
	}
	for _, marker := range markers {
		if marker.Value == nil || marker.Status == domain.StatusUnknown || marker.Confidence < 0.7 {
			return true
		}
	}
	return false
}

func ClassifyAnalysis(markers []domain.Marker, text string) string {
	lower := strings.ToLower(text)
	hasAny := func(words ...string) bool {
		for _, word := range words {
			if strings.Contains(lower, word) {
				return true
			}
		}
		return false
	}
	if hasAny("компьютерная томография", "кт органов", "кт-признак") {
		if hasAny("грудной полост", "грудной клет", "легк") {
			return "КТ · органы грудной клетки"
		}
		return "Компьютерная томография"
	}
	if hasAny("ультразвуковое исследование", "протокол ультразвукового", "эхоскопически") {
		if hasAny("щитовидн") {
			return "УЗИ · щитовидная железа"
		}
		if hasAny("почки", "почек", "почечн") {
			return "УЗИ · почки"
		}
		return "Ультразвуковое исследование"
	}
	cbcMarkers, hormoneMarkers, urineMarkers := 0, 0, 0
	for _, marker := range markers {
		switch strings.ToLower(marker.CanonicalName) {
		case "hemoglobin", "erythrocytes", "leukocytes", "platelets", "hematocrit", "mcv", "mch", "mchc", "esr":
			cbcMarkers++
		case "tsh", "free_t4", "free_t3", "cortisol", "prolactin", "testosterone", "estradiol", "insulin":
			hormoneMarkers++
		case "urine_protein", "urine_glucose", "urine_leukocytes", "urine_erythrocytes", "urine_ph", "specific_gravity":
			urineMarkers++
		}
	}
	if urineMarkers >= 2 || hasAny("общий анализ моч", "удельный вес", "плоский эпител", "лейкоциты в моч", "цвет моч", "микроальбумин") {
		if hasAny("микроальбумин", "суточная моч", "белок в моч", "креатинин моч") {
			return "Моча · биохимия"
		}
		return "Моча · ОАМ"
	}
	if cbcMarkers >= 2 || hasAny("гемоглобин", "эритроцит", "лейкоцит", "тромбоцит", "гематокрит", "соэ") {
		return "Кровь · ОАК"
	}
	if hormoneMarkers >= 2 || hasAny("ттг", "тиреотроп", "т4 свобод", "т3 свобод", "кортизол", "пролактин", "тестостерон", "эстрадиол", "инсулин") {
		return "Кровь · гормоны"
	}
	biochemistry := 0
	for _, marker := range markers {
		switch marker.CanonicalName {
		case "glucose", "albumin", "bilirubin_total", "bilirubin_direct", "alt", "ast", "cholesterol_total", "triglycerides", "hdl", "ldl", "urea", "creatinine", "egfr", "crp", "uric_acid", "iron", "calcium_total", "potassium", "sodium":
			biochemistry++
		}
	}
	if biochemistry >= 2 || hasAny("биохимический анализ крови") {
		return "Кровь · биохимия"
	}
	return "Лабораторное исследование"
}

// ExtractStudyReport handles narrative diagnostic reports without asking a
// language model to reconstruct medical facts. It copies only text present in
// the document and leaves a visible warning when the photographed fragment has
// no formal conclusion.
func ExtractStudyReport(text string) *domain.StudyReport {
	clean := strings.ReplaceAll(text, "\r", "")
	lower := strings.ToLower(strings.ReplaceAll(clean, "ё", "е"))
	report := &domain.StudyReport{Warnings: []string{}}
	switch {
	case strings.Contains(lower, "компьютерн") && (strings.Contains(lower, "томограф") || strings.Contains(lower, "кт-признак")):
		report.Modality = "КТ"
		report.StudyName = "Компьютерная томография"
		if strings.Contains(lower, "грудн") || strings.Contains(lower, "легк") {
			report.StudyName = "Компьютерная томография органов грудной клетки"
		}
	case strings.Contains(lower, "ультразвук") || strings.Contains(lower, "эхоскопически"):
		report.Modality = "УЗИ"
		report.StudyName = "Ультразвуковое исследование"
		if strings.Contains(lower, "щитовидн") {
			report.StudyName = "Ультразвуковое исследование щитовидной железы"
		} else if strings.Contains(lower, "почки") || strings.Contains(lower, "почек") {
			report.StudyName = "Ультразвуковое исследование почек"
		}
	default:
		return nil
	}

	conclusionStart := findHeading(lower, "заключение")
	if conclusionStart >= 0 {
		after := clean[conclusionStart:]
		if colon := strings.IndexAny(after, ":\n"); colon >= 0 {
			after = after[colon+1:]
		}
		report.Conclusion = trimReportSection(after, []string{"врач:", "врач ", "сформировал", "данное заключение", "результат подтвердил"})
	}

	start := reportDescriptionStart(lower, report.Modality)
	end := len(clean)
	if conclusionStart >= 0 && conclusionStart > start {
		end = conclusionStart
	}
	if start >= 0 && start < end {
		report.Description = trimReportSection(clean[start:end], []string{"врач:", "врач ", "результат подтвердил"})
	}
	if len([]rune(report.Description)) < 40 {
		report.Description = ""
		report.Warnings = append(report.Warnings, "Описание исследования распознано не полностью — сверьте с оригиналом.")
	}
	if report.Conclusion == "" {
		report.Warnings = append(report.Warnings, "Заключение отсутствует в предоставленном фрагменте или не распознано.")
	}
	report.Confidence = 0.72
	if report.Description != "" {
		report.Confidence += 0.1
	}
	if report.Conclusion != "" {
		report.Confidence += 0.1
	}
	return report
}

func findHeading(lower, heading string) int {
	re := regexp.MustCompile(`(?m)(?:^|\n)\s*` + regexp.QuoteMeta(heading) + `\s*:?`)
	match := re.FindStringIndex(lower)
	if match == nil {
		return -1
	}
	return match[0]
}

func reportDescriptionStart(lower, modality string) int {
	patterns := []string{"протокол:", "протокол ", "щитовидная железа расположена", "почки\n", "почки \n"}
	if modality == "КТ" {
		patterns = []string{"протокол:", "протокол "}
	}
	best := -1
	for _, pattern := range patterns {
		if index := strings.Index(lower, pattern); index >= 0 && (best < 0 || index < best) {
			best = index
		}
	}
	return best
}

func trimReportSection(value string, stops []string) string {
	lower := strings.ToLower(strings.ReplaceAll(value, "ё", "е"))
	end := len(value)
	for _, stop := range stops {
		if index := strings.Index(lower, stop); index >= 0 && index < end {
			end = index
		}
	}
	lines := strings.Split(value[:end], "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func NormalizeConfirmedReport(report *domain.StudyReport) (*domain.StudyReport, error) {
	if report == nil {
		return nil, fmt.Errorf("missing report")
	}
	normalized := *report
	normalized.Modality = strings.TrimSpace(normalized.Modality)
	normalized.StudyName = strings.TrimSpace(normalized.StudyName)
	normalized.Description = strings.TrimSpace(normalized.Description)
	normalized.Conclusion = strings.TrimSpace(normalized.Conclusion)
	if normalized.StudyName == "" || len([]rune(normalized.StudyName)) > 240 || len([]rune(normalized.Description)) > 20000 || len([]rune(normalized.Conclusion)) > 4000 {
		return nil, fmt.Errorf("invalid report")
	}
	if normalized.Description == "" && normalized.Conclusion == "" {
		return nil, fmt.Errorf("empty report")
	}
	normalized.Confidence = 1
	normalized.Warnings = nil
	return &normalized, nil
}

func (s *Service) ReviewStudyReportForPatient(ctx context.Context, report *domain.StudyReport, profile *domain.PatientProfile) domain.AIReview {
	summary := "Описание исследования сохранено. Формальное заключение в предоставленном фрагменте отсутствует; сверьте документ с оригиналом и обсудите его с лечащим врачом."
	if strings.TrimSpace(report.Conclusion) != "" {
		summary = "В заключении исследования указано: " + strings.TrimSpace(report.Conclusion)
	}
	return domain.AIReview{
		Summary: summary, Lifestyle: []string{}, Nutrition: []string{}, DoctorNeeded: true, Urgency: "routine",
		Disclaimer: "Это пересказ подтверждённого текста исследования, а не диагноз. Интерпретировать результат должен лечащий врач с учётом симптомов и анамнеза.", Provider: "rules",
	}
}

// ExtractCollectedAt prefers specimen collection / study dates and ignores
// birth and print dates commonly present on the same laboratory sheet.
func ExtractCollectedAt(text string) *time.Time {
	normalized := strings.ToLower(strings.ReplaceAll(text, "\n", " "))
	patterns := []string{
		`дата\s+(?:взятия|забора)(?:\s+биоматериала|\s+материала)?\s*[:.]?\s*(\d{1,2}[./-]\d{1,2}[./-]\d{4})`,
		`дата\s+(?:проведения\s+)?исследования\s*[:.]?\s*(\d{1,2}[./-]\d{1,2}[./-]\d{4})`,
		`дата\s+сдачи\s*[:.]?\s*(\d{1,2}[./-]\d{1,2}[./-]\d{4})`,
	}
	for _, pattern := range patterns {
		match := regexp.MustCompile(pattern).FindStringSubmatch(normalized)
		if len(match) != 2 {
			continue
		}
		value := strings.NewReplacer("/", ".", "-", ".").Replace(match[1])
		if parsed, err := time.Parse("2.1.2006", value); err == nil && parsed.Year() >= 1900 && !parsed.After(time.Now().Add(24*time.Hour)) {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func NormalizeConfirmedMarkers(markers []domain.Marker) ([]domain.Marker, error) {
	if len(markers) == 0 || len(markers) > 200 {
		return nil, fmt.Errorf("invalid marker count")
	}
	out := make([]domain.Marker, 0, len(markers))
	for _, marker := range markers {
		marker.Name = strings.TrimSpace(marker.Name)
		if marker.Name == "" || len(marker.Name) > 160 {
			return nil, fmt.Errorf("invalid marker name")
		}
		if marker.Value != nil && (math.IsNaN(*marker.Value) || math.IsInf(*marker.Value, 0)) {
			return nil, fmt.Errorf("invalid marker value")
		}
		marker.TextValue = strings.TrimSpace(marker.TextValue)
		if len([]rune(marker.TextValue)) > 300 {
			return nil, fmt.Errorf("invalid marker text value")
		}
		marker.Confidence = 1
		marker.Warnings = nil
		marker.Status = inferStatus(marker)
		out = append(out, marker)
	}
	return out, nil
}

func inferStatus(marker domain.Marker) domain.MarkerStatus {
	if marker.Value == nil {
		value := strings.ToLower(strings.ReplaceAll(marker.TextValue, "ё", "е"))
		if strings.Contains(value, "+") {
			return domain.StatusHigh
		}
		if strings.Contains(value, "отриц") || strings.Contains(value, "норма") {
			return domain.StatusNormal
		}
		return domain.StatusUnknown
	}
	if marker.ReferenceMin != nil && *marker.Value < *marker.ReferenceMin {
		return domain.StatusLow
	}
	if marker.ReferenceMax != nil && *marker.Value > *marker.ReferenceMax {
		return domain.StatusHigh
	}
	if marker.ReferenceMin != nil || marker.ReferenceMax != nil {
		return domain.StatusNormal
	}
	return marker.Status
}
func (s *Service) extract(ctx context.Context, path, mime string) (string, []string, error) {
	if strings.Contains(mime, "pdf") {
		b, e := exec.CommandContext(ctx, "pdftotext", "-layout", path, "-").Output()
		if e == nil && len(bytes.TrimSpace(b)) > 20 {
			return string(b), []string{string(b)}, nil
		}
		tmpDir, e := os.MkdirTemp("", "lab-pdf-ocr-")
		if e != nil {
			return "", nil, e
		}
		defer os.RemoveAll(tmpDir)
		prefix := filepath.Join(tmpDir, "page")
		// Ограничение в 10 страниц защищает API от чрезмерно тяжёлых PDF.
		cmd := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", "200", "-f", "1", "-l", "10", path, prefix)
		if output, renderErr := cmd.CombinedOutput(); renderErr != nil {
			return "", nil, fmt.Errorf("render pdf: %v: %s", renderErr, output)
		}
		pages, e := filepath.Glob(prefix + "-*.png")
		if e != nil || len(pages) == 0 {
			return "", nil, fmt.Errorf("render pdf: no pages produced")
		}
		var text strings.Builder
		var passTexts []strings.Builder
		for _, page := range pages {
			pageText, pageCandidates, ocrErr := s.extractImageDetailed(ctx, page)
			if ocrErr != nil {
				return "", nil, fmt.Errorf("tesseract pdf page: %w", ocrErr)
			}
			text.WriteString(pageText)
			text.WriteString("\n")
			for i, candidate := range pageCandidates {
				for len(passTexts) <= i {
					passTexts = append(passTexts, strings.Builder{})
				}
				passTexts[i].WriteString(candidate)
				passTexts[i].WriteString("\n")
			}
		}
		combined := text.String()
		candidates := make([]string, 0, len(passTexts))
		for i := range passTexts {
			candidates = append(candidates, passTexts[i].String())
		}
		if len(candidates) == 0 {
			candidates = []string{combined}
		}
		return combined, candidates, nil
	}
	return s.extractImageDetailed(ctx, path)
}

func (s *Service) extractImage(ctx context.Context, path string) (string, error) {
	text, _, err := s.extractImageDetailed(ctx, path)
	return text, err
}

func (s *Service) extractImageDetailed(ctx context.Context, path string) (string, []string, error) {
	ocrPath, cleanup, preprocessErr := preprocessImage(ctx, path)
	if cleanup != nil {
		defer cleanup()
	}
	if preprocessErr != nil {
		log.Printf("image preprocessing failed, using original: %v", preprocessErr)
		ocrPath = path
	}
	runPass := func(psm, language string) (string, error) {
		cmd := exec.CommandContext(ctx, "tesseract", ocrPath, "stdout", "-l", language, "--psm", psm, "-c", "preserve_interword_spaces=1")
		b, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("psm %s: %v: %s", psm, err, b)
		}
		return string(b), nil
	}
	// PSM 4 стабильно читает табличные лабораторные бланки. На небольшом
	// production-сервере одновременные проходы конкурируют за CPU, поэтому
	// дорогой PSM 11 запускаем только когда основной проход извлёк мало полей.
	// Russian lab forms are both faster and more accurately spaced with the
	// single Russian model. If that pass cannot find a table, the fallback uses
	// the complete configured language set so English and mixed forms remain
	// supported.
	primary, primaryErr := runPass("4", primaryOCRLanguage(s.cfg.TesseractLang))
	candidates := make([]string, 0, 2)
	if strings.TrimSpace(primary) != "" {
		candidates = append(candidates, primary)
	}
	primaryMarkers := parseMarkers(primary)
	// The sparse pass costs about as much as the primary OCR pass. It is useful
	// when table mode loses whole rows, but not when a mostly complete table only
	// has an uncertain reference: DeepSeek structuring handles that later.
	primaryReport := ExtractStudyReport(primary)
	primaryIncomplete := len(primaryMarkers) < 8 && (primaryReport == nil || len(strings.Fields(primaryReport.Description)) < 30)
	if primaryErr != nil || primaryIncomplete {
		fallback, fallbackErr := runPass("6", s.cfg.TesseractLang)
		if strings.TrimSpace(fallback) != "" {
			candidates = append(candidates, fallback)
		}
		if fallbackErr == nil && ExtractStudyReport(fallback) == nil {
			sparse, _ := runPass("11", s.cfg.TesseractLang)
			if strings.TrimSpace(sparse) != "" {
				candidates = append(candidates, sparse)
			}
		}
		if len(candidates) == 0 {
			if fallbackErr != nil {
				return "", nil, fallbackErr
			}
			return "", nil, primaryErr
		}
	}
	if len(candidates) == 0 {
		return "", nil, fmt.Errorf("tesseract returned empty text")
	}
	best := candidates[0]
	bestScore := ocrScore(best)
	for _, candidate := range candidates[1:] {
		if score := ocrScore(candidate); score > bestScore {
			best = candidate
			bestScore = score
		}
	}
	return best, candidates, nil
}

func primaryOCRLanguage(configured string) string {
	for _, language := range strings.Split(configured, "+") {
		if strings.TrimSpace(language) == "rus" {
			return "rus"
		}
	}
	return configured
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
	// PGM avoids the comparatively expensive PNG encoder/decoder round trip.
	// It is lossless and is read natively by Tesseract/Leptonica.
	outputPath := filepath.Join(tmpDir, "normalized.pgm")
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
	for _, term := range []string{"глюкоз", "альбумин", "билирубин", "креатинин", "мочевин", "холестерин", "лейкоцит", "эритроцит", "гемоглобин", "тромбоцит", "анализ моч", "заключение", "ультразвук", "томограф"} {
		if strings.Contains(lower, term) {
			labTerms++
		}
	}
	reportScore := 0
	if report := ExtractStudyReport(text); report != nil {
		reportScore = len(strings.Fields(report.Description))*80 + len(strings.Fields(report.Conclusion))*500
	}
	return len(parseMarkers(text))*10000 + decimalValues*200 + labTerms*100 + reportScore + len(strings.Fields(text))
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
	{"Триглицериды", "triglycerides", "ммоль/л", []string{"триглицериды", "триглицерид"}, 100},
	{"ЛПВП", "hdl", "ммоль/л", []string{"лпвп", "nnen"}, 20},
	{"ЛПНП", "ldl", "ммоль/л", []string{"лпнп", "аннп", "anhn"}, 30},
	{"Мочевина", "urea", "ммоль/л", []string{"мочевин"}, 100},
	{"Креатинин", "creatinine", "мкмоль/л", []string{"креатинин"}, 2000},
	{"СКФ", "egfr", "мл/мин/1,73 м²", []string{"фильтрац"}, 200},
	{"СРБ", "crp", "мг/л", []string{"срб"}, 1000},
	{"Мочевая кислота", "uric_acid", "мкмоль/л", []string{"мочевая кислота", "mouebas кислота"}, 2000},
	{"Железо", "iron", "мкмоль/л", []string{"железо"}, 200},
	{"Кальций общий", "calcium_total", "ммоль/л", []string{"кальций"}, 10},
	{"Калий", "potassium", "ммоль/л", []string{"калий"}, 20},
	{"Натрий", "sodium", "ммоль/л", []string{"натрий"}, 200},
	{"СОЭ", "esr", "мм/ч", []string{"соэ по панченкову", "соэпо панченкову", "соэ"}, 200},
	{"Нейтрофилы палочкоядерные", "band_neutrophils", "%", []string{"нейтрофилы палочкоядерные", "палочкоядерные"}, 100},
	{"Нейтрофилы сегментоядерные", "segmented_neutrophils", "%", []string{"нейтрофилы сегментоядерные", "сегментоядерные"}, 100},
	{"Эозинофилы", "eosinophils", "%", []string{"эозинофилы"}, 100},
	{"Моноциты", "monocytes", "%", []string{"моноциты ручной подсчет", "моноциты"}, 100},
	{"Лимфоциты", "lymphocytes", "%", []string{"лимфоциты"}, 100},
	{"Лейкоциты (WBC)", "leukocytes", "10^9/л", []string{"wbc"}, 1000},
	{"Эритроциты (RBC)", "erythrocytes", "10^12/л", []string{"rbc"}, 100},
	{"Гемоглобин (HGB)", "hemoglobin", "г/л", []string{"hgb", "гемоглобин"}, 1000},
	{"Гематокрит (HCT)", "hematocrit", "л/л", []string{"hct", "гематокрит"}, 10},
	{"Тромбоциты (PLT)", "platelets", "10^9/л", []string{"plt", "тромбоциты"}, 5000},
	{"Лимфоциты (LYM%)", "lymphocytes_percent", "%", []string{"lym%"}, 100},
	{"Моноциты (MON%)", "monocytes_percent", "%", []string{"mon%"}, 100},
	{"Гранулоциты (GRAN%)", "granulocytes_percent", "%", []string{"gran%"}, 100},
	{"Микроальбумин", "urine_microalbumin", "мг/сут", []string{"микроальбумин"}, 10000},
	{"pH мочи", "urine_ph", "", []string{"ph"}, 14},
	{"Относительная плотность", "specific_gravity", "", []string{"относительная плотность", "удельный вес"}, 2},
}

var (
	row              = regexp.MustCompile(`(?m)^[ \t]*([\p{L}][\p{L} \t\-()/]{2,}?)[ \t]+([<>]?[ \t]*\d+(?:[.,]\d+)?)[ \t]*([%\p{L}/^\d]*)[ \t]*(?:[ \t]+([\d.,]+)[ \t]*[-–][ \t]*([\d.,]+))?[ \t]*$`)
	numberToken      = regexp.MustCompile(`(?:>=|<=|>|<)?[ \t]*\d+(?:[.,]\d+)?`)
	rangeToken       = regexp.MustCompile(`(\d+(?:[.,]\d+)?)[ \t]*[-–=][ \t]*(\d+(?:[.,]\d+)?)`)
	thresholdToken   = regexp.MustCompile(`(>=|<=|>|<)[ \t]*(\d+(?:[.,]\d+)?)`)
	leadingParenCode = regexp.MustCompile(`^[ \t]*\([^)]{0,12}(?:\)|[ \t]+)`)
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
		mk := domain.Marker{Name: name, CanonicalName: canonical, Value: &v, Unit: strings.TrimSpace(m[3]), Status: domain.StatusUnknown, Confidence: 0.55, Warnings: []string{"Неизвестный показатель — проверьте название и значение."}}
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
	for _, line := range markerSegments(text) {
		synthetic := strings.HasPrefix(line, syntheticSegmentPrefix)
		line = strings.TrimPrefix(line, syntheticSegmentPrefix)
		lower := strings.ToLower(strings.ReplaceAll(line, "ё", "е"))
		for _, spec := range markerSpecs {
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
			rawValue := value
			value = normalizeMarkerValue(value, numbers[0], spec.maxPlausible)
			marker := domain.Marker{Name: spec.name, CanonicalName: spec.canonical, Value: floatPtr(value), Unit: spec.unit, Status: domain.StatusUnknown, Confidence: 0.74}
			if synthetic {
				marker.Confidence = 0.52
				marker.Warnings = append(marker.Warnings, "Значение собрано из раздельных строк OCR — обязательно сверьте с оригиналом.")
			}
			if value != rawValue {
				marker.Warnings = append(marker.Warnings, "OCR потерял десятичный разделитель результата.")
				marker.Confidence = 0.62
			}
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
					rawA, rawB := a, b
					a, b = normalizeReferenceRange(value, a, b, hint, spec.unit)
					marker.ReferenceMin, marker.ReferenceMax = floatPtr(a), floatPtr(b)
					marker.ReferenceText = formatReference(a, b)
					marker.Status = statusForRange(value, a, b)
					marker.Confidence = maxFloat(marker.Confidence, 0.86)
					if a != rawA || b != rawB {
						marker.Warnings = append(marker.Warnings, "OCR потерял десятичный разделитель референса.")
						marker.Confidence = minFloat(marker.Confidence, 0.76)
					}
				}
			} else if match := thresholdToken.FindStringSubmatch(after); len(match) == 3 {
				threshold, okThreshold := parseOCRNumber(match[2])
				if okThreshold {
					marker.ReferenceText = match[1] + " " + formatNumber(threshold)
					marker.Confidence = maxFloat(marker.Confidence, 0.86)
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
			} else if len(numbers) > 1 {
				if minValue, maxValue, warning, ok := repairCollapsedReference(spec.canonical, value, numbers[1:]); ok {
					marker.ReferenceMin, marker.ReferenceMax = floatPtr(minValue), floatPtr(maxValue)
					marker.ReferenceText = formatReference(minValue, maxValue)
					marker.Status = statusForRange(value, minValue, maxValue)
					marker.Confidence = maxFloat(marker.Confidence, 0.76)
					marker.Warnings = appendUnique(marker.Warnings, warning)
				}
			}
			if hint != domain.StatusUnknown {
				marker.Status = hint
			}
			if spec.canonical == "crp" && marker.ReferenceMin == nil && marker.ReferenceMax == nil && marker.Status == domain.StatusUnknown {
				continue
			}
			if marker.Value != nil && marker.ReferenceMin != nil && marker.ReferenceMax != nil {
				// A sparse OCR pass can mistake the first reference boundary for
				// the result. Treat an exact boundary as uncertain instead of using
				// it to overrule a clearer table-mode pass.
				if !strings.ContainsAny(numbers[0], ",.") && (*marker.Value == *marker.ReferenceMin || *marker.Value == *marker.ReferenceMax) {
					marker.Confidence = minFloat(marker.Confidence, 0.64)
					marker.Warnings = appendUnique(marker.Warnings, "Результат совпал с границей референса — проверьте строку.")
				}
				// A missing decimal comma can also occur in the result column.
				// Only repair an integer that is implausibly far beyond the same
				// row's upper reference; ordinary high results remain untouched.
				if !strings.ContainsAny(numbers[0], ",.") && *marker.ReferenceMax > 0 && *marker.Value > *marker.ReferenceMax*5 {
					fixed := *marker.Value
					for fixed > *marker.ReferenceMax*2 && fixed >= 10 {
						fixed /= 10
					}
					if fixed != *marker.Value {
						marker.Value = floatPtr(fixed)
						marker.Status = statusForRange(fixed, *marker.ReferenceMin, *marker.ReferenceMax)
						marker.Confidence = minFloat(marker.Confidence, 0.62)
						marker.Warnings = appendUnique(marker.Warnings, "OCR потерял десятичный разделитель результата.")
					}
				}
			}
			if synthetic {
				marker.Confidence = minFloat(marker.Confidence, 0.58)
				if spec.unit == "%" {
					marker.ReferenceMin, marker.ReferenceMax, marker.ReferenceText = nil, nil, ""
					marker.Status = domain.StatusUnknown
				}
			}
			if existing, exists := found[spec.canonical]; !exists || markerScore(marker) > markerScore(existing) {
				found[spec.canonical] = marker
			}
		}
	}
	out := make([]domain.Marker, 0, len(found))
	for _, spec := range markerSpecs {
		if marker, ok := found[spec.canonical]; ok {
			out = append(out, marker)
		}
	}
	for _, marker := range parseUrinalysisTextMarkers(text) {
		if _, exists := found[marker.CanonicalName]; !exists {
			out = append(out, marker)
		}
	}
	return out
}

func parseUrinalysisTextMarkers(text string) []domain.Marker {
	lowerDocument := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	if !strings.Contains(lowerDocument, "анализ моч") && !strings.Contains(lowerDocument, "осадка моч") && !strings.Contains(lowerDocument, "относительная плотность") {
		return nil
	}
	type textSpec struct {
		name, canonical string
		aliases         []string
		visual          bool
	}
	specs := []textSpec{
		{"Аскорбиновая кислота", "urine_ascorbic_acid", []string{"аскорбиновая кислота"}, false},
		{"Нитриты", "urine_nitrites", []string{"нитриты"}, false},
		{"Эритроциты", "urine_erythrocytes", []string{"эритроциты"}, false},
		{"Лейкоциты", "urine_leukocytes", []string{"лейкоциты"}, false},
		{"Кетоновые тела", "urine_ketones", []string{"кетоновые тела"}, false},
		{"Уробилиноген", "urine_urobilinogen", []string{"уробилиноген"}, false},
		{"Билирубин", "urine_bilirubin", []string{"билирубин"}, false},
		{"Глюкоза", "urine_glucose", []string{"глюкоза"}, false},
		{"Белок", "urine_protein", []string{"белок"}, false},
		{"Бактерии", "urine_bacteria", []string{"бактерии"}, false},
		{"Слизь", "urine_mucus", []string{"слизь"}, false},
		{"Прозрачность", "urine_clarity", []string{"прозрачность"}, true},
		{"Цвет", "urine_color", []string{"цвет"}, true},
		{"Эпителий плоский", "urine_squamous_epithelium", []string{"эпителий плоский"}, true},
	}
	lines := strings.Split(text, "\n")
	found := map[string]domain.Marker{}
	for _, rawLine := range lines {
		line := strings.Join(strings.Fields(rawLine), " ")
		lower := strings.ToLower(strings.ReplaceAll(line, "ё", "е"))
		for _, spec := range specs {
			alias, index := matchedAlias(lower, spec.aliases)
			if index < 0 {
				continue
			}
			value := strings.Trim(strings.TrimSpace(line[index+len(alias):]), ":;|-—")
			if value == "" {
				continue
			}
			marker := domain.Marker{Name: spec.name, CanonicalName: spec.canonical, TextValue: value, Status: domain.StatusUnknown, Confidence: 0.8}
			valueLower := strings.ToLower(strings.ReplaceAll(value, "ё", "е"))
			if strings.Contains(value, "+") {
				marker.Status = domain.StatusHigh
			} else if !spec.visual && (strings.Contains(valueLower, "отриц") || strings.Contains(valueLower, "норма")) {
				marker.Status = domain.StatusNormal
			}
			if regexp.MustCompile(`\d+\s*[-–]\s*\d+`).MatchString(value) {
				marker.Unit = "в п/зр"
				marker.Status = domain.StatusUnknown
				if marker.CanonicalName == "urine_leukocytes" {
					marker.Name = "Лейкоциты (микроскопия)"
					marker.CanonicalName = "urine_leukocytes_microscopy"
				}
			}
			if existing, exists := found[marker.CanonicalName]; !exists || len([]rune(marker.TextValue)) > len([]rune(existing.TextValue)) {
				found[marker.CanonicalName] = marker
			}
		}
	}
	out := make([]domain.Marker, 0, len(found))
	for _, spec := range specs {
		if marker, ok := found[spec.canonical]; ok {
			out = append(out, marker)
		}
	}
	if marker, ok := found["urine_leukocytes_microscopy"]; ok {
		out = append(out, marker)
	}
	return out
}

// repairCollapsedReference handles only OCR artifacts confirmed on photographed
// laboratory forms. It is deliberately marker-specific: a generic split of a
// four-digit token could silently invent a reference interval.
func repairCollapsedReference(canonical string, value float64, tokens []string) (float64, float64, string, bool) {
	if len(tokens) == 0 {
		return 0, 0, "", false
	}
	first := strings.TrimSpace(tokens[0])
	switch canonical {
	case "iron":
		if first == "1026" && value >= 0 && value <= 100 {
			return 10, 26, "OCR восстановил слитый референс железа — проверьте строку.", true
		}
	case "calcium_total":
		// Depending on the thresholding result, the same printed `2 - 2,6`
		// has appeared as `2256`, `2725` and, on the production fixture, `225`
		// after the final 6 was confused with 5. The measured-value guard and
		// marker-specific branch prevent applying this repair to unrelated rows.
		if (first == "225" || first == "2256" || first == "2725") && value >= 2 && value <= 3 {
			return 2, 2.6, "OCR восстановил слитый референс кальция — проверьте строку.", true
		}
	case "uric_acid":
		// A blurred decimal comma and dash may disappear independently:
		// `154,7 - 428,4` then becomes the two tokens `1547 4284`.
		if len(tokens) > 1 && first == "1547" && strings.TrimSpace(tokens[1]) == "4284" && value >= 100 && value <= 1000 {
			return 154.7, 428.4, "OCR восстановил разделители референса мочевой кислоты — проверьте строку.", true
		}
	}
	return 0, 0, "", false
}

// PSM 11 often preserves values more accurately than table mode, but emits each
// cell on a separate line. Synthetic segments join a marker heading with the
// following cells until the next marker heading, allowing both layouts to use
// the same strict parser without joining unrelated rows.
const syntheticSegmentPrefix = "\x1f"

func markerSegments(text string) []string {
	raw := strings.Split(text, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	segments := append([]string{}, lines...)
	for i, line := range lines {
		canonical := markerCanonicalInLine(line)
		if canonical == "" {
			continue
		}
		parts := []string{line}
		for j := i + 1; j < len(lines) && j <= i+10; j++ {
			nextCanonical := markerCanonicalInLine(lines[j])
			if nextCanonical != "" && nextCanonical != canonical {
				break
			}
			if nextCanonical == canonical {
				// Many reports print a bold group heading followed by the actual
				// result row. Restart at the result label so symbols from the
				// heading (for example "(0" misread from "(K)") cannot become
				// the measured value.
				parts = []string{lines[j]}
				continue
			}
			parts = append(parts, lines[j])
		}
		if len(parts) > 1 {
			segments = append(segments, syntheticSegmentPrefix+strings.Join(parts, " "))
		}
	}
	return segments
}

func markerCanonicalInLine(line string) string {
	lower := strings.ToLower(strings.ReplaceAll(line, "ё", "е"))
	for _, spec := range markerSpecs {
		if _, index := matchedAlias(lower, spec.aliases); index >= 0 {
			return spec.canonical
		}
	}
	return ""
}

func markerScore(marker domain.Marker) int {
	score := 0
	if marker.Value != nil {
		score += 8
	}
	if marker.ReferenceMin != nil || marker.ReferenceMax != nil {
		score += 5
	}
	if marker.Status != domain.StatusUnknown {
		score += 3
	}
	if marker.Unit != "" {
		score += 2
	}
	score += int(marker.Confidence * 10)
	return score
}

func parseOCRCandidates(texts []string) []domain.Marker {
	if len(texts) == 0 {
		return []domain.Marker{}
	}
	selected := map[string]domain.Marker{}
	order := []string{}
	for pass, text := range texts {
		for _, marker := range parseMarkers(text) {
			current, exists := selected[marker.CanonicalName]
			if !exists {
				order = append(order, marker.CanonicalName)
				selected[marker.CanonicalName] = marker
				continue
			}
			if markerValuesAgree(current, marker) {
				if current.ReferenceMin == nil && current.ReferenceMax == nil && marker.Confidence >= 0.7 {
					current.ReferenceMin, current.ReferenceMax, current.ReferenceText = marker.ReferenceMin, marker.ReferenceMax, marker.ReferenceText
					if marker.ReferenceMin != nil || marker.ReferenceMax != nil {
						current.Status = marker.Status
					}
				}
				current.Confidence = maxFloat(current.Confidence, minFloat(0.96, marker.Confidence+0.06))
				current.Warnings = appendUnique(current.Warnings, marker.Warnings...)
				selected[marker.CanonicalName] = current
				continue
			}
			if current.Confidence < 0.7 && marker.Confidence >= 0.7 && pass > 0 {
				marker.Warnings = appendUnique(marker.Warnings, "Режимы OCR прочитали значение по-разному — сверьте с оригиналом.")
				marker.Confidence = minFloat(marker.Confidence, 0.68)
				selected[marker.CanonicalName] = marker
			} else {
				current.Confidence = minFloat(current.Confidence, 0.58)
				current.Warnings = appendUnique(current.Warnings, "Режимы OCR прочитали значение по-разному — сохранён результат табличного режима.")
				selected[marker.CanonicalName] = current
			}
		}
	}
	out := make([]domain.Marker, 0, len(order))
	for _, canonical := range order {
		out = append(out, selected[canonical])
	}
	return out
}

func markerValuesAgree(a, b domain.Marker) bool {
	if a.Value == nil || b.Value == nil {
		return a.Value == nil && b.Value == nil && strings.EqualFold(strings.TrimSpace(a.TextValue), strings.TrimSpace(b.TextValue))
	}
	tolerance := maxFloat(0.011, maxFloat(absFloat(*a.Value), absFloat(*b.Value))*0.002)
	return absFloat(*a.Value-*b.Value) <= tolerance
}

func appendUnique(values []string, additions ...string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			values, seen[value] = append(values, value), true
		}
	}
	return values
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func matchedAlias(line string, aliases []string) (string, int) {
	for _, alias := range aliases {
		if len([]rune(alias)) <= 3 && !strings.ContainsAny(alias, " -_/()") {
			pattern := regexp.MustCompile(`(?i)(^|[^\p{L}])(` + regexp.QuoteMeta(alias) + `)(?:[^\p{L}]|$)`)
			if match := pattern.FindStringSubmatchIndex(line); match != nil {
				return line[match[4]:match[5]], match[4]
			}
			continue
		}
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

func normalizeReferenceRange(value, rawMin, rawMax float64, hint domain.MarkerStatus, unit string) (float64, float64) {
	// Percent ranges such as 50–70 are already naturally bounded. Dividing them
	// to force the measured value inside the range would silently turn a real
	// low/high flag into “normal”.
	if unit == "%" && rawMin >= 0 && rawMax <= 100 {
		return rawMin, rawMax
	}
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
			distance := 0.0
			if value < minValue {
				distance = (minValue - value) / maxFloat(absFloat(value), 1)
			}
			if value > maxValue {
				distance = (value - maxValue) / maxFloat(absFloat(value), 1)
			}
			if hint == domain.StatusHigh && value <= maxValue {
				distance += 10
			}
			if hint == domain.StatusLow && value >= minValue {
				distance += 10
			}
			width := (maxValue - minValue) / maxFloat(absFloat(value), 1)
			score := distance*100 + width*0.01 + float64(minScale+maxScale)*0.02
			if score < bestScore {
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
		if marker.Value == nil || marker.Status == domain.StatusUnknown || marker.Unit == "" || marker.Confidence < 0.7 {
			return true
		}
	}
	return false
}

func normalizeExternalMarkers(markers []domain.Marker) []domain.Marker {
	out := make([]domain.Marker, 0, len(markers))
	seen := map[string]bool{}
	for _, marker := range markers {
		var spec *markerSpec
		for i := range markerSpecs {
			if marker.CanonicalName == markerSpecs[i].canonical || isAliasFor(marker.Name, markerSpecs[i]) {
				spec = &markerSpecs[i]
				break
			}
		}
		if spec == nil || marker.Value == nil || *marker.Value < 0 || *marker.Value > spec.maxPlausible || seen[spec.canonical] {
			continue
		}
		seen[spec.canonical] = true
		marker.Name, marker.CanonicalName, marker.Unit = spec.name, spec.canonical, spec.unit
		marker.Confidence = 0.48
		marker.Warnings = appendUnique(marker.Warnings, "Поле дополнено языковой моделью — обязательно сверьте с оригиналом.")
		if marker.ReferenceMin != nil && marker.ReferenceMax != nil {
			marker.Status = statusForRange(*marker.Value, *marker.ReferenceMin, *marker.ReferenceMax)
		} else {
			marker.Status = domain.StatusUnknown
		}
		out = append(out, marker)
	}
	return out
}

func isAliasFor(name string, spec markerSpec) bool {
	lower := strings.ToLower(strings.ReplaceAll(name, "ё", "е"))
	_, index := matchedAlias(lower, append(spec.aliases, strings.ToLower(spec.name)))
	return index >= 0
}

func mergeMarkerSets(local, external []domain.Marker) []domain.Marker {
	out := append([]domain.Marker{}, local...)
	indices := map[string]int{}
	for i, marker := range out {
		indices[marker.CanonicalName] = i
	}
	for _, candidate := range external {
		i, exists := indices[candidate.CanonicalName]
		if !exists {
			indices[candidate.CanonicalName] = len(out)
			out = append(out, candidate)
			continue
		}
		current := out[i]
		if !markerValuesAgree(current, candidate) {
			current.Confidence = minFloat(current.Confidence, 0.58)
			current.Warnings = appendUnique(current.Warnings, "OCR и языковая модель предложили разные значения.")
			out[i] = current
			continue
		}
		if current.ReferenceMin == nil && candidate.ReferenceMin != nil {
			current.ReferenceMin = candidate.ReferenceMin
		}
		if current.ReferenceMax == nil && candidate.ReferenceMax != nil {
			current.ReferenceMax = candidate.ReferenceMax
		}
		if current.ReferenceText == "" {
			current.ReferenceText = candidate.ReferenceText
		}
		if current.Status == domain.StatusUnknown && current.Value != nil && current.ReferenceMin != nil && current.ReferenceMax != nil {
			current.Status = statusForRange(*current.Value, *current.ReferenceMin, *current.ReferenceMax)
		}
		out[i] = current
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
func (s *Service) deepSeek(ctx context.Context, text string, profile *domain.PatientProfile) ([]domain.Marker, domain.AIReview, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	profileContext := "Профиль пациента не заполнен."
	if profile != nil {
		profileContext = fmt.Sprintf("Возраст %d лет, рост %.0f см, вес %.1f кг, ИМТ %.1f.", profile.Age, profile.HeightCM, profile.WeightKG, profile.BMI)
	}
	payload := map[string]any{
		"model":           s.cfg.DeepSeekModel,
		"temperature":     0.1,
		"max_tokens":      4096,
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"messages":        []msg{{"system", "Ты медицинский модуль структурирования лабораторных бланков. Верни только json-объект: markers (поля name, canonical_name, value, text_value, unit, reference_min, reference_max, reference_text, status low|normal|high|unknown) и ai_review (summary, lifestyle[], nutrition[], doctor_needed, urgency routine|soon|urgent, suggested_specialty). Summary должен быть кратким и понятным пациенту, учитывать возраст и ИМТ, отмечать отклонения и при их наличии рекомендовать профиль специалиста. Не ставь диагноз, не назначай препараты, не выдумывай отсутствующие значения. ИМТ используй только как контекст, а не как диагноз."}, {"user", profileContext + "\n\nТекст бланка:\n" + text}},
	}
	content, e := s.requestDeepSeek(ctx, payload)
	if e != nil {
		return nil, domain.AIReview{}, e
	}
	var out struct {
		Markers  []domain.Marker `json:"markers"`
		AIReview domain.AIReview `json:"ai_review"`
	}
	if e = json.Unmarshal([]byte(content), &out); e != nil {
		return nil, domain.AIReview{}, e
	}
	out.AIReview.Provider = "deepseek"
	out.AIReview.Disclaimer = "Автоматическая оценка не является диагнозом и не заменяет консультацию врача. При резком ухудшении самочувствия обратитесь за неотложной помощью."
	return out.Markers, out.AIReview, nil
}

func (s *Service) deepSeekReview(ctx context.Context, markers []domain.Marker, profile *domain.PatientProfile) (domain.AIReview, error) {
	profileContext := "Профиль пользователя не заполнен."
	if profile != nil {
		profileContext = fmt.Sprintf("Возраст %d лет, рост %.0f см, вес %.1f кг, ИМТ %.1f.", profile.Age, profile.HeightCM, profile.WeightKG, profile.BMI)
	}
	compact := make([]map[string]any, 0, len(markers))
	for _, marker := range markers {
		item := map[string]any{"name": marker.Name, "unit": marker.Unit, "status": marker.Status, "reference": marker.ReferenceText}
		if marker.Value != nil {
			item["value"] = *marker.Value
		} else if marker.TextValue != "" {
			item["value"] = marker.TextValue
		}
		if marker.ReferenceMin != nil {
			item["reference_min"] = *marker.ReferenceMin
		}
		if marker.ReferenceMax != nil {
			item["reference_max"] = *marker.ReferenceMax
		}
		compact = append(compact, item)
	}
	markerJSON, err := json.Marshal(compact)
	if err != nil {
		return domain.AIReview{}, err
	}
	var out struct {
		AIReview domain.AIReview `json:"ai_review"`
	}
	system := "Ты медицинский модуль краткого резюме лабораторных результатов. Верни только JSON {ai_review:{summary,lifestyle:[],nutrition:[],doctor_needed,urgency,suggested_specialty}}. Summary: 2–4 понятных предложения только о содержании анализа и отклонениях, без диагноза и назначения препаратов. Учитывай возраст и ИМТ как контекст. Если есть значимые отклонения, укажи подходящую специальность врача. Не повторяй таблицу и не выдумывай данные."
	if err = s.completeJSON(ctx, system, profileContext+"\nПоказатели: "+string(markerJSON), &out); err != nil {
		return domain.AIReview{}, err
	}
	if strings.TrimSpace(out.AIReview.Summary) == "" {
		return domain.AIReview{}, fmt.Errorf("deepseek returned empty review")
	}
	out.AIReview.Provider = "deepseek"
	out.AIReview.Disclaimer = ruleReview(markers).Disclaimer
	if out.AIReview.Lifestyle == nil {
		out.AIReview.Lifestyle = []string{}
	}
	if out.AIReview.Nutrition == nil {
		out.AIReview.Nutrition = []string{}
	}
	return out.AIReview, nil
}

type SymptomResult struct {
	Accepted  bool   `json:"accepted"`
	Title     string `json:"title"`
	Answer    string `json:"answer"`
	Specialty string `json:"specialty"`
}

type ClinicalAssistResult struct {
	Assessment      string   `json:"assessment"`
	RedFlags        []string `json:"red_flags"`
	SuggestedChecks []string `json:"suggested_checks"`
	Tactics         []string `json:"tactics"`
	GuidelineRefs   []string `json:"guideline_refs"`
	Limitations     string   `json:"limitations"`
}

type PatientHealthSummaryResult struct {
	Summary string `json:"summary"`
}

func (s *Service) PatientHealthSummary(ctx context.Context, patient domain.User, analyses []domain.Analysis) PatientHealthSummaryResult {
	analyses = completedAnalyses(analyses)
	if len(analyses) == 0 {
		return PatientHealthSummaryResult{Summary: "После загрузки и распознавания анализов здесь появится общее резюме вашего текущего состояния и динамики показателей."}
	}
	fallback := "По доступным исследованиям сформировано общее резюме. "
	if text := strings.TrimSpace(analyses[0].AIReview.Summary); text != "" {
		fallback += text
	} else {
		fallback += "Оцените результаты вместе с врачом с учётом самочувствия и истории здоровья."
	}
	if len(analyses) > 1 {
		fallback += " В истории есть несколько исследований; повторяющиеся показатели следует оценивать в динамике по датам и референсным диапазонам лабораторий."
	}
	if s.cfg.DeepSeekAPIKey == "" {
		return PatientHealthSummaryResult{Summary: fallback}
	}
	profileJSON, _ := json.Marshal(patient.PatientProfile)
	var out PatientHealthSummaryResult
	system := "Ты формируешь одно общее безопасное резюме состояния пациента по всей доступной истории лабораторных исследований. Не перечисляй отдельные резюме анализов подряд. Сначала кратко опиши общую картину, затем оцени динамику только тех показателей, которые действительно измерялись неоднократно. Учитывай даты и разные референсные диапазоны, не ставь диагноз, не назначай препараты и не выдумывай отсутствующие данные. При значимых отклонениях укажи, к какому врачу разумно обратиться. Ответ на русском, понятный пациенту, 5–9 предложений. Верни JSON {summary}."
	err := s.completeJSON(ctx, system, "Профиль: "+string(profileJSON)+"\nИстория анализов:\n"+compactAnalysisContext(analyses), &out)
	if err != nil || strings.TrimSpace(out.Summary) == "" {
		return PatientHealthSummaryResult{Summary: fallback}
	}
	return out
}

func (s *Service) Recommendation(ctx context.Context, kind string, profile domain.PatientProfile, analyses []domain.Analysis) (string, error) {
	fallback := "Поддерживайте регулярный режим и меняйте привычки постепенно. Индивидуальные ограничения и интенсивность нагрузок согласуйте с врачом."
	if kind == "nutrition" {
		fallback = "Старайтесь питаться регулярно, чаще выбирать овощи и продукты с клетчаткой, а избыток жирной пищи и быстрых углеводов уменьшать постепенно. Индивидуальную диету согласуйте с врачом."
	}
	if s.cfg.DeepSeekAPIKey == "" {
		return fallback, nil
	}
	survey, _ := json.Marshal(profile)
	context := compactAnalysisContext(analyses)
	system := "Ты формируешь безопасную персональную рекомендацию по образу жизни для пациента. Используй возраст, ИМТ, ответы анкеты и только перечисленные лабораторные данные. Не ставь диагноз, не назначай лекарства и не предлагай экстремальные нагрузки или диеты. Ответ на русском, 3-5 коротких конкретных пунктов и отдельная строка, когда нужна очная консультация."
	if kind == "nutrition" {
		system = "Ты формируешь безопасную персональную рекомендацию по питанию. Используй возраст, ИМТ, ответы анкеты и только перечисленные лабораторные данные. Не ставь диагноз, не назначай лекарства и не предлагай лечебную или экстремальную диету. Ответ на русском, 3-5 коротких конкретных пунктов и отдельная строка, когда нужна консультация врача или диетолога."
	}
	var out struct {
		Recommendation string `json:"recommendation"`
	}
	err := s.completeJSON(ctx, system+" Верни JSON {recommendation}.", "Профиль и анкеты: "+string(survey)+"\nПоследние анализы: "+context, &out)
	if err != nil || strings.TrimSpace(out.Recommendation) == "" {
		if err == nil {
			err = fmt.Errorf("empty recommendation")
		}
		return fallback, err
	}
	return out.Recommendation, nil
}

func (s *Service) SymptomConsultation(ctx context.Context, profile *domain.PatientProfile, question string, analyses []domain.Analysis) (SymptomResult, error) {
	if s.cfg.DeepSeekAPIKey == "" {
		return SymptomResult{}, fmt.Errorf("ai service is not configured")
	}
	profileJSON, _ := json.Marshal(profile)
	system := "Ты выполняешь только первичную безопасную маршрутизацию жалоб пациента. Определи, относится ли текст к состоянию здоровья. Если нет, accepted=false и в answer ровно сообщи, что принимается только информация о состоянии здоровья. Если да: кратко объясни разумные следующие действия, тревожные признаки для срочной помощи и к какому одному профильному специалисту обратиться. Не ставь диагноз, не назначай препараты и дозировки. Учитывай профиль и лабораторные данные, но не делай причинных выводов только по ним. Верни JSON: accepted, title (до 60 символов), answer, specialty."
	var out SymptomResult
	err := s.completeJSON(ctx, system, "Профиль: "+string(profileJSON)+"\nПоследние анализы: "+compactAnalysisContext(analyses)+"\nСообщение пациента: "+strings.TrimSpace(question), &out)
	return out, err
}

func (s *Service) ClinicalAssist(ctx context.Context, patient domain.User, objective, clinical string, analyses []domain.Analysis) (ClinicalAssistResult, error) {
	if s.cfg.DeepSeekAPIKey == "" {
		return ClinicalAssistResult{}, fmt.Errorf("ai service is not configured")
	}
	profileJSON, _ := json.Marshal(patient.PatientProfile)
	system := "Ты модуль поддержки клинического решения только для врача. Работай исключительно с предоставленными данными, явно отмечай недостаток информации и не выдумывай факты. Сформируй дифференциальное клиническое рассуждение, красные флаги, что уточнить/проверить и осторожную тактику без назначения конкретных препаратов и доз. Ссылки указывай только как название организации, документа и год; если не уверен в актуальности или точном документе, не придумывай ссылку и напиши, что врачу нужно свериться с действующей редакцией официального источника. Не утверждай, что ответ заменяет клиническое решение. Верни JSON: assessment, red_flags[], suggested_checks[], tactics[], guideline_refs[], limitations."
	user := "Профиль пациента: " + string(profileJSON) + "\nОбъективные данные: " + strings.TrimSpace(objective) + "\nКлинические данные: " + strings.TrimSpace(clinical) + "\nДоступные анализы:\n" + compactAnalysisContext(analyses)
	var out ClinicalAssistResult
	err := s.completeJSON(ctx, system, user, &out)
	if strings.TrimSpace(out.Limitations) == "" {
		out.Limitations = "Рекомендация носит справочный характер: проверьте её по действующей редакции клинических рекомендаций и сопоставьте с полной клинической картиной."
	}
	return out, err
}

func (s *Service) DoctorChat(ctx context.Context, messages []domain.AIMessage) (string, error) {
	if s.cfg.DeepSeekAPIKey == "" {
		return "", fmt.Errorf("ai service is not configured")
	}
	if len(messages) > 20 {
		messages = messages[len(messages)-20:]
	}
	conversation, _ := json.Marshal(messages)
	var out struct {
		Reply string `json:"reply"`
	}
	system := "Ты — AI-помощник врача для клинического рассуждения. Отвечай по-русски, структурированно и кратко. Не выдумывай данные пациента, исследования, ссылки или актуальность рекомендаций. Явно отделяй факты от гипотез, отмечай красные флаги и недостающие данные. Не заменяй решение врача и не назначай конкретные препараты или дозировки. Если ссылаешься на руководство, проси сверить действующую редакцию в официальном источнике. Верни JSON с полем reply."
	err := s.completeJSON(ctx, system, "История диалога: "+string(conversation), &out)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Reply) == "" {
		return "", fmt.Errorf("empty ai reply")
	}
	return strings.TrimSpace(out.Reply), nil
}

func compactAnalysisContext(analyses []domain.Analysis) string {
	analyses = completedAnalyses(analyses)
	if len(analyses) == 0 {
		return "нет данных"
	}
	if len(analyses) > 3 {
		analyses = analyses[:3]
	}
	var lines []string
	for _, analysis := range analyses {
		var markers []string
		for _, marker := range analysis.Markers {
			if marker.Status != domain.StatusNormal || len(markers) < 6 {
				value := marker.TextValue
				if marker.Value != nil {
					value = fmt.Sprintf("%g", *marker.Value)
				}
				markers = append(markers, fmt.Sprintf("%s=%s %s (%s)", marker.Name, value, marker.Unit, marker.Status))
			}
		}
		lines = append(lines, analysis.CreatedAt.Format("02.01.2006")+" "+analysis.Title+": "+strings.Join(markers, ", "))
	}
	return strings.Join(lines, "\n")
}

func completedAnalyses(analyses []domain.Analysis) []domain.Analysis {
	completed := make([]domain.Analysis, 0, len(analyses))
	for _, analysis := range analyses {
		if analysis.Status == domain.AnalysisStatusReady {
			completed = append(completed, analysis)
		}
	}
	return completed
}

func (s *Service) completeJSON(ctx context.Context, system, user string, out any) error {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	payload := map[string]any{
		"model":           s.cfg.DeepSeekModel,
		"temperature":     0.1,
		"max_tokens":      1400,
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"messages":        []msg{{"system", system}, {"user", user}},
	}
	content, err := s.requestDeepSeek(ctx, payload)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(content), out)
}
