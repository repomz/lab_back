package analyzer

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
)

func TestParseMarkers(t *testing.T) {
	m := parseMarkers("Глюкоза 6,2 ммоль/л 3,9 - 5,5\nГемоглобин 140 г/л 120 - 160")
	if len(m) != 2 {
		t.Fatalf("got %d", len(m))
	}
	if string(m[0].Status) != "high" {
		t.Fatalf("status %s", m[0].Status)
	}
	if string(m[1].Status) != "normal" {
		t.Fatalf("status %s", m[1].Status)
	}
}

func TestCompleteJSONUsesDeepSeekMessageFieldNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		messages := payload["messages"].([]any)
		first := messages[0].(map[string]any)
		if first["role"] != "system" || first["content"] != "system prompt" {
			t.Fatalf("wrong message JSON: %#v", first)
		}
		if _, exists := first["Role"]; exists {
			t.Fatalf("DeepSeek message fields must be lowercase: %#v", first)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"answer\":\"ok\"}"}}]}`))
	}))
	defer server.Close()
	service := New(config.Config{DeepSeekAPIKey: "test", DeepSeekBaseURL: server.URL, DeepSeekModel: "test"})
	var result struct {
		Answer string `json:"answer"`
	}
	if err := service.completeJSON(context.Background(), "system prompt", "user prompt", &result); err != nil {
		t.Fatal(err)
	}
	if result.Answer != "ok" {
		t.Fatalf("answer=%q", result.Answer)
	}
}

func TestEmptyReviewDoesNotClaimRecognition(t *testing.T) {
	r := emptyReview()
	if r.Summary == "" || r.Provider != "rules" {
		t.Fatalf("unexpected review: %#v", r)
	}
}

func TestOCRScorePrefersStructuredLabText(t *testing.T) {
	structured := "Глюкоза 6,2 ммоль/л 3,9 - 5,5"
	noisy := "лабораторный результат пациента содержит много отдельных слов без значений"
	if ocrScore(structured) <= ocrScore(noisy) {
		t.Fatal("structured OCR result must have a higher score")
	}
}

func TestPrimaryOCRLanguagePrefersFastRussianModel(t *testing.T) {
	if got := primaryOCRLanguage("rus+eng"); got != "rus" {
		t.Fatalf("primary language = %q, want rus", got)
	}
	if got := primaryOCRLanguage("eng"); got != "eng" {
		t.Fatalf("primary language = %q, want eng", got)
	}
}

func TestParsePhotographedLabTable(t *testing.T) {
	text := `
Глюкоза 4,77 39-64 ммоль/л
Альбумин 44,60 36 - 50 г/л
Билирубин общий 5,83 3,4 - 20,5 мкмоль/л
Билирубин прямой 3,10 0-51 мкмоль/л
АЛТ 9,50 0-40 Ед/л
ACT 13,60 0-40 Ед/л
Холестерин общий 5,03 31-65 ммоль/л
Триглицериды (1) 1,40 0,55-1,65 ммоль/л
nnen 1,69 0,78-2,07 ммоль/л
лПНП 2,70 1,81-492 ммоль/л
*> Мочевина 8,39 33-83 ммоль/л
*> Креатинин 136,86 44-97 мкмоль/л
фильтрации (СКФ) по 32 >= 60
СРБ 0,0 0-10 мг/л
*> Мочевая кислота 517,60 1547-428,4 мкмоль/л
Железо 11,91 10-26 мкмоль/л
Кальций общий 2330 2256 ммоль/л
Калий 4,40 36-55 ммоль/л
Натрий 144,0 135-150 ммоль/л`

	markers := parseMarkers(text)
	if len(markers) != 19 {
		t.Fatalf("got %d markers: %#v", len(markers), markers)
	}
	byName := map[string]domain.Marker{}
	for _, marker := range markers {
		byName[marker.CanonicalName] = marker
	}
	assertMarker := func(name string, value, minValue, maxValue float64, status domain.MarkerStatus) {
		t.Helper()
		marker, ok := byName[name]
		if !ok || marker.Value == nil || math.Abs(*marker.Value-value) > 0.001 {
			t.Fatalf("unexpected %s marker: %#v", name, marker)
		}
		if minValue >= 0 && (marker.ReferenceMin == nil || math.Abs(*marker.ReferenceMin-minValue) > 0.001) {
			t.Fatalf("unexpected %s min: %#v", name, marker.ReferenceMin)
		}
		if maxValue >= 0 && (marker.ReferenceMax == nil || math.Abs(*marker.ReferenceMax-maxValue) > 0.001) {
			t.Fatalf("unexpected %s max: %#v", name, marker.ReferenceMax)
		}
		if marker.Status != status {
			t.Fatalf("unexpected %s status: %s", name, marker.Status)
		}
	}
	assertMarker("glucose", 4.77, 3.9, 6.4, domain.StatusNormal)
	assertMarker("bilirubin_direct", 3.10, 0, 5.1, domain.StatusNormal)
	assertMarker("cholesterol_total", 5.03, 3.1, 6.5, domain.StatusNormal)
	assertMarker("urea", 8.39, 3.3, 8.3, domain.StatusHigh)
	assertMarker("creatinine", 136.86, 44, 97, domain.StatusHigh)
	assertMarker("egfr", 32, 60, -1, domain.StatusLow)
	assertMarker("uric_acid", 517.60, 154.7, 428.4, domain.StatusHigh)
	assertMarker("calcium_total", 2.33, 2, 2.6, domain.StatusNormal)
	assertMarker("potassium", 4.40, 3.6, 5.5, domain.StatusNormal)
	if markersNeedReview(markers) {
		t.Fatal("the photographed table should be complete after deterministic repairs")
	}
}

func TestParseSparseOCRCells(t *testing.T) {
	text := `
"Кальций общий
Кальций (Ca)
2,330
2-26
ммоль/л
"Калий
Калий (K)
4,40
36-55
ммоль/л
"Натрий
144,0
135 = 150
ммоль/л`
	markers := parseMarkers(text)
	byName := map[string]domain.Marker{}
	for _, marker := range markers {
		byName[marker.CanonicalName] = marker
	}
	if len(markers) != 3 {
		t.Fatalf("got %d markers: %#v", len(markers), markers)
	}
	if got := *byName["calcium_total"].Value; math.Abs(got-2.33) > 0.001 {
		t.Fatalf("calcium=%g", got)
	}
	if got := *byName["calcium_total"].ReferenceMax; math.Abs(got-2.6) > 0.001 {
		t.Fatalf("calcium max=%g", got)
	}
	if got := *byName["potassium"].Value; math.Abs(got-4.4) > 0.001 {
		t.Fatalf("potassium=%g", got)
	}
	if got := *byName["sodium"].ReferenceMax; math.Abs(got-150) > 0.001 {
		t.Fatalf("sodium max=%g", got)
	}
}

func TestRepairsCollapsedReferencesFromFastRussianOCR(t *testing.T) {
	markers := parseMarkers("Мочевая кислота 517,60 1547 4284 мкмоль/л\nЖелезо (Fe) 11,91 1026 мкмоль/л\nКальций общий 2330 2725 ммоль/л")
	byName := map[string]domain.Marker{}
	for _, marker := range markers {
		byName[marker.CanonicalName] = marker
	}
	assertRange := func(name string, wantMin, wantMax float64, wantStatus domain.MarkerStatus) {
		t.Helper()
		marker, ok := byName[name]
		if !ok || marker.ReferenceMin == nil || marker.ReferenceMax == nil {
			t.Fatalf("missing repaired range for %s: %#v", name, marker)
		}
		if math.Abs(*marker.ReferenceMin-wantMin) > 0.001 || math.Abs(*marker.ReferenceMax-wantMax) > 0.001 {
			t.Fatalf("unexpected repaired range for %s: %#v", name, marker)
		}
		if marker.Status != wantStatus {
			t.Fatalf("unexpected repaired status for %s: %s", name, marker.Status)
		}
	}
	assertRange("uric_acid", 154.7, 428.4, domain.StatusHigh)
	assertRange("iron", 10, 26, domain.StatusNormal)
	assertRange("calcium_total", 2, 2.6, domain.StatusNormal)
}

func TestRepairsCalciumReferenceFromProductionOCR(t *testing.T) {
	markers := parseMarkers(`"Кальций общий                                    2330                      225              нноле/л
Кальций (Са) _                                  -`)
	if len(markers) != 1 {
		t.Fatalf("got %d markers: %#v", len(markers), markers)
	}
	marker := markers[0]
	if marker.CanonicalName != "calcium_total" || marker.Value == nil || marker.ReferenceMin == nil || marker.ReferenceMax == nil {
		t.Fatalf("calcium reference was not restored: %#v", marker)
	}
	if math.Abs(*marker.Value-2.33) > 0.001 || math.Abs(*marker.ReferenceMin-2) > 0.001 || math.Abs(*marker.ReferenceMax-2.6) > 0.001 {
		t.Fatalf("unexpected calcium marker: %#v", marker)
	}
	if marker.Status != domain.StatusNormal {
		t.Fatalf("unexpected calcium status: %s", marker.Status)
	}
}

func TestMultipleOCRPassesDoNotLetShiftedColumnsOverrideClearRow(t *testing.T) {
	clear := "Альбумин 44,60 36-50 г/л\nБилирубин общий 5,83 3,4-20,5 мкмоль/л"
	sparse := "Альбумин\n36\n36-50\nг/л\nБилирубин общий\n583\n3,4-20,5\nмкмоль/л"
	markers := parseOCRCandidates([]string{clear, sparse})
	byName := map[string]domain.Marker{}
	for _, marker := range markers {
		byName[marker.CanonicalName] = marker
	}
	if got := *byName["albumin"].Value; math.Abs(got-44.6) > 0.001 {
		t.Fatalf("albumin=%g", got)
	}
	if got := *byName["bilirubin_total"].Value; math.Abs(got-5.83) > 0.001 {
		t.Fatalf("bilirubin=%g", got)
	}
}

func TestOCRGroupCodeIsNotUsedAsTriglycerideValue(t *testing.T) {
	markers := parseMarkers("Триглицериды (1) 1,40 0,55-1,65 ммоль/л")
	if len(markers) != 1 || markers[0].Value == nil || math.Abs(*markers[0].Value-1.4) > 0.001 {
		t.Fatalf("unexpected triglycerides: %#v", markers)
	}
}

func TestParseCBCFromPhotographedForm(t *testing.T) {
	text := `
СОЭ по Панченкову 21,0 2 - 15 мм/час
Нейтрофилы палочкоядерные 0 1 - 5 %
Нейтрофилы сегментоядерные 47 50 - 70 %
Эозинофилы 3 2 - 4 %
Моноциты ручной подсчёт 3 2 - 8 %
Лимфоциты 47 25 - 40 %
WBC 6,70 4 - 10 10^9/л
RBC 4,55 3,5 - 5,5 10^12/л
HGB 130,0 г/л
HCT 0,387 0,35 - 0,5
PLT 270,0 150 - 320 10^9/л
LYM% 52,70 20 - 40 %
MON% 4,90 3 - 15 %
GRAN% 42,40 50 - 70 %`
	markers := parseMarkers(text)
	byName := map[string]domain.Marker{}
	for _, marker := range markers {
		byName[marker.CanonicalName] = marker
	}
	for _, name := range []string{"esr", "band_neutrophils", "segmented_neutrophils", "leukocytes", "erythrocytes", "hemoglobin", "platelets", "lymphocytes_percent", "granulocytes_percent"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("missing %s in %#v", name, markers)
		}
	}
	if byName["esr"].Status != domain.StatusHigh || byName["band_neutrophils"].Status != domain.StatusLow || byName["granulocytes_percent"].Status != domain.StatusLow {
		t.Fatalf("wrong CBC statuses: ESR=%s bands=%s GRAN=%s", byName["esr"].Status, byName["band_neutrophils"].Status, byName["granulocytes_percent"].Status)
	}
}

func TestParseUrinalysisQualitativeValues(t *testing.T) {
	text := `
Микроскопическое исследование осадка мочи
Эпителий плоский 1-2
Бактерии +
Слизь +
Лейкоциты 6-8
Общий анализ мочи
pH 6.0
Аскорбиновая кислота Отрицательно
Нитриты Отрицательно
Эритроциты Отрицательно
Лейкоциты ++++
Кетоновые тела Отрицательно
Уробилиноген Норма
Билирубин Отрицательно
Глюкоза Норма
Белок Отрицательно
Относительная плотность 1.015
Прозрачность Слегка мутная
Цвет Соломенно-желтый`
	markers := parseMarkers(text)
	byName := map[string]domain.Marker{}
	for _, marker := range markers {
		byName[marker.CanonicalName] = marker
	}
	if len(markers) < 14 {
		t.Fatalf("only %d urine fields: %#v", len(markers), markers)
	}
	if byName["urine_leukocytes"].TextValue != "++++" || byName["urine_leukocytes"].Status != domain.StatusHigh {
		t.Fatalf("wrong urine leukocytes: %#v", byName["urine_leukocytes"])
	}
	if byName["urine_nitrites"].Status != domain.StatusNormal || byName["urine_clarity"].Status != domain.StatusUnknown {
		t.Fatalf("wrong qualitative statuses: %#v %#v", byName["urine_nitrites"], byName["urine_clarity"])
	}
}

func TestExtractNarrativeStudiesWithoutInventingConclusion(t *testing.T) {
	ct := `Компьютерная томография органов грудной полости
Дата исследования: 25.08.2026
Протокол: Легочные поля симметричные. Инфильтративных изменений не выявлено.
Заключение: КТ-признаки пневмофиброза, перенесенной ТЭЛА справа, ЛАГ
Врач: специалист`
	report := ExtractStudyReport(ct)
	if report == nil || report.Modality != "КТ" || !strings.Contains(report.Conclusion, "пневмофиброза") || strings.Contains(report.Description, "Дата исследования") {
		t.Fatalf("unexpected CT report: %#v", report)
	}
	ultrasound := `ПРОТОКОЛ УЛЬТРАЗВУКОВОГО ИССЛЕДОВАНИЯ
Ультразвуковое исследование щитовидной железы и региональных лимфатических узлов
Щитовидная железа расположена обычно. Выявляются узлы: 6,5x8,0x9,0 мм.`
	report = ExtractStudyReport(ultrasound)
	if report == nil || report.Conclusion != "" || len(report.Warnings) == 0 {
		t.Fatalf("missing-conclusion report must remain explicit: %#v", report)
	}
}

func TestExplicitDecimalZeroAtReferenceBoundaryRemainsCredible(t *testing.T) {
	markers := parseOCRCandidates([]string{"СРБ 0,0 0-10 мг/л", "СРБ\n0,0\n0-10\nмг/л"})
	if len(markers) != 1 || markers[0].Confidence < 0.85 || markers[0].Status != domain.StatusNormal {
		t.Fatalf("unexpected CRP marker: %#v", markers)
	}
}

func TestExternalModelCannotOverrideConflictingLocalValue(t *testing.T) {
	value, wrong := 4.77, 47.7
	local := []domain.Marker{{Name: "Глюкоза", CanonicalName: "glucose", Value: &value, Unit: "ммоль/л", Status: domain.StatusNormal, Confidence: 0.94}}
	external := normalizeExternalMarkers([]domain.Marker{{Name: "Глюкоза", CanonicalName: "glucose", Value: &wrong}})
	merged := mergeMarkerSets(local, external)
	if len(merged) != 1 || merged[0].Value == nil || *merged[0].Value != value {
		t.Fatalf("unexpected merge: %#v", merged)
	}
	if merged[0].Confidence >= local[0].Confidence {
		t.Fatal("a disagreement must lower confidence")
	}
}

func TestSparseOrUncertainMarkersNeedDeepSeekStructuring(t *testing.T) {
	value := 4.77
	if !markersNeedStructuring([]domain.Marker{{Name: "Глюкоза", CanonicalName: "glucose", Value: &value, Confidence: 0.94, Status: domain.StatusNormal, ReferenceText: "3.9–6.4"}}) {
		t.Fatal("a sparse table must be sent for structuring")
	}
	complete := make([]domain.Marker, 8)
	for i := range complete {
		complete[i] = domain.Marker{Name: "Показатель", CanonicalName: "marker", Value: &value, Confidence: 0.94, Status: domain.StatusNormal, ReferenceText: "1–10"}
	}
	if markersNeedStructuring(complete) {
		t.Fatal("a complete trusted table only needs a review")
	}
	complete[3].Confidence = 0.62
	if !markersNeedStructuring(complete) {
		t.Fatal("an uncertain row must be sent for structuring")
	}
	longTable := append(complete, complete[:4]...)
	if markersNeedStructuring(longTable) {
		t.Fatal("a long table must go directly to patient verification")
	}
}

func TestExtractCollectedAtPrefersSpecimenDate(t *testing.T) {
	text := "Дата рождения: 05.10.1948 Дата взятия материала: 23.03.2026 10:57 Дата и время печати: 30.03.2026"
	got := ExtractCollectedAt(text)
	if got == nil || got.Format("02.01.2006") != "23.03.2026" {
		t.Fatalf("unexpected collection date: %v", got)
	}
}

func TestOCRPhotoFixture(t *testing.T) {
	path := os.Getenv("LAB_OCR_FIXTURE")
	if path == "" {
		t.Skip("LAB_OCR_FIXTURE is not set")
	}
	service := New(config.Config{TesseractLang: "rus+eng"})
	text, candidates, err := service.extract(context.Background(), path, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	markers := parseOCRCandidates(candidates)
	if len(markers) != 19 {
		t.Fatalf("recognized %d markers instead of 19; OCR text: %.500s", len(markers), text)
	}
	byName := map[string]domain.Marker{}
	for _, marker := range markers {
		byName[marker.CanonicalName] = marker
	}
	for canonical, expected := range map[string]float64{"glucose": 4.77, "creatinine": 136.86, "egfr": 32, "uric_acid": 517.6, "sodium": 144} {
		if byName[canonical].Value == nil || *byName[canonical].Value != expected {
			t.Fatalf("required marker %s = %v, want %v", canonical, byName[canonical].Value, expected)
		}
	}
}

func TestClassifyAnalysisFromRecognizedMarkers(t *testing.T) {
	biochemistry := []domain.Marker{{CanonicalName: "glucose"}, {CanonicalName: "creatinine"}, {CanonicalName: "alt"}}
	if got := ClassifyAnalysis(biochemistry, "Сыворотка крови"); got != "Кровь · биохимия" {
		t.Fatalf("biochemistry classified as %q", got)
	}
	cbc := []domain.Marker{{CanonicalName: "hemoglobin"}, {CanonicalName: "leukocytes"}, {CanonicalName: "platelets"}}
	if got := ClassifyAnalysis(cbc, "Общий анализ крови"); got != "Кровь · ОАК" {
		t.Fatalf("CBC classified as %q", got)
	}
	if got := ClassifyAnalysis(nil, "Общий анализ мочи лейкоциты эритроциты удельный вес"); got != "Моча · ОАМ" {
		t.Fatalf("urinalysis classified as %q", got)
	}
}
