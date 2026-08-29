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
	return s.ProcessForPatient(ctx, path, mime, nil)
}

func (s *Service) ProcessForPatient(ctx context.Context, path, mime string, profile *domain.PatientProfile) (string, []domain.Marker, domain.AIReview, string) {
	text, candidates, err := s.extract(ctx, path, mime)
	if err != nil {
		return "", []domain.Marker{}, failedReview(), "failed"
	}
	markers := parseOCRCandidates(candidates)
	review := ruleReview(markers)
	if s.cfg.DeepSeekAPIKey != "" && strings.TrimSpace(text) != "" {
		if m, r, e := s.deepSeek(ctx, text, profile); e != nil {
			log.Printf("deepseek structuring failed: %v", e)
		} else if len(m) > 0 {
			// An external language model may fill gaps, but it must never replace a
			// stronger value that was read consistently by local OCR passes.
			markers = mergeMarkerSets(markers, normalizeExternalMarkers(m))
			fallback := ruleReview(markers)
			if strings.TrimSpace(r.Summary) == "" {
				r = fallback
			}
			r.Provider = "deepseek"
			r.Disclaimer = fallback.Disclaimer
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
	if urineMarkers >= 2 || hasAny("общий анализ моч", "удельный вес", "плоский эпител", "лейкоциты в моч", "цвет моч") {
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
			return "", nil, lastErr
		}
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
	{"Триглицериды", "triglycerides", "ммоль/л", []string{"триглицериды", "триглицерид"}, 100},
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
					a, b = normalizeReferenceRange(value, a, b, hint)
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
			}
			if hint != domain.StatusUnknown {
				marker.Status = hint
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
	return out
}

// PSM 11 often preserves values more accurately than table mode, but emits each
// cell on a separate line. Synthetic segments join a marker heading with the
// following cells until the next marker heading, allowing both layouts to use
// the same strict parser without joining unrelated rows.
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
			segments = append(segments, strings.Join(parts, " "))
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
	byCanonical := map[string][]domain.Marker{}
	order := []string{}
	for _, text := range texts {
		for _, marker := range parseMarkers(text) {
			if _, exists := byCanonical[marker.CanonicalName]; !exists {
				order = append(order, marker.CanonicalName)
			}
			byCanonical[marker.CanonicalName] = append(byCanonical[marker.CanonicalName], marker)
		}
	}
	out := make([]domain.Marker, 0, len(order))
	for _, canonical := range order {
		candidates := byCanonical[canonical]
		bestIndex, bestVotes, bestScore := 0, 0, -1
		credibleCount := 0
		for _, candidate := range candidates {
			if candidate.Confidence >= 0.7 {
				credibleCount++
			}
		}
		for i, candidate := range candidates {
			votes := 0
			for _, other := range candidates {
				if other.Confidence >= 0.7 && markerValuesAgree(candidate, other) {
					votes++
				}
			}
			score := votes*100 + markerScore(candidate)
			if score > bestScore {
				bestIndex, bestVotes, bestScore = i, votes, score
			}
		}
		best := candidates[bestIndex]
		for _, candidate := range candidates {
			if markerValuesAgree(best, candidate) && markerScore(candidate) > markerScore(best) {
				candidate.Warnings = appendUnique(candidate.Warnings, best.Warnings...)
				best = candidate
			}
		}
		if bestVotes >= 2 {
			best.Confidence = maxFloat(best.Confidence, minFloat(0.98, 0.84+float64(bestVotes)*0.06))
		}
		if credibleCount > 1 && bestVotes < credibleCount {
			best.Confidence = minFloat(best.Confidence, 0.58)
			best.Warnings = appendUnique(best.Warnings, "Режимы OCR прочитали значение по-разному.")
		}
		out = append(out, best)
	}
	return out
}

func markerValuesAgree(a, b domain.Marker) bool {
	if a.Value == nil || b.Value == nil {
		return a.Value == nil && b.Value == nil
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

func compactAnalysisContext(analyses []domain.Analysis) string {
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
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.DeepSeekBaseURL, "/")+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.DeepSeekAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("deepseek status %d", resp.StatusCode)
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&envelope); err != nil || len(envelope.Choices) == 0 {
		return fmt.Errorf("invalid deepseek response")
	}
	return json.Unmarshal([]byte(envelope.Choices[0].Message.Content), out)
}
