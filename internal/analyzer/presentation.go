package analyzer

import (
	"regexp"
	"strings"

	"github.com/repomz/lab_back/internal/domain"
)

// CanonicalAnalysisIdentity is the single source of truth for patient-facing
// names and rubric sections. Provider-generated labels are deliberately not
// used as categories: they vary between otherwise identical documents.
func CanonicalAnalysisIdentity(markers []domain.Marker, medicalText string, report *domain.StudyReport) (string, string) {
	search := normalizedAnalysisText(markers, medicalText, report)
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(search, normalizeSearch(part)) {
				return true
			}
		}
		return false
	}

	if report != nil || has("компьютерная томография", "кт-признак", "ультразвуковое исследование", "эхоскопически", "рентген", "мрт") {
		modality := normalizeSearch(reportValue(report, func(value *domain.StudyReport) string { return value.Modality }))
		switch {
		case modality == "кт" || has("компьютерная томография", "кт-признак"):
			if has("грудной полости", "грудной клетки", "легкие", "легких") {
				return "КТ органов грудной клетки", "КТ и МРТ"
			}
			return "Компьютерная томография", "КТ и МРТ"
		case modality == "мрт" || has("магнитно-резонанс"):
			return "Магнитно-резонансная томография", "КТ и МРТ"
		case modality == "рентген" || has("рентген"):
			return "Рентгенография", "Рентген"
		default:
			if has("щитовидная железа", "щитовидной железы") {
				return "УЗИ щитовидной железы", "УЗИ"
			}
			if has("почки", "почек", "почечный") {
				return "УЗИ почек", "УЗИ"
			}
			return "Ультразвуковое исследование", "УЗИ"
		}
	}

	counts := markerFamilies(markers)
	if counts["thyroid"] >= 1 || has("тиреотропный", "ттг", "т3 свобод", "т4 свобод") {
		return "Гормоны щитовидной железы", "Кровь"
	}
	if has("общий анализ мочи", "осадка мочи", "относительная плотность") {
		return "Общий анализ мочи", "Моча"
	}
	if counts["cbc"] >= 1 || has("общий анализ крови", "лейкоцитарная формула", "соэ по панченкову") {
		return "Общий анализ крови", "Кровь"
	}
	if counts["urine"] >= 1 || has("общий анализ мочи", "осадка мочи", "относительная плотность", "бактерии слизь") {
		if has("микроальбумин", "microalbumin") {
			return "Микроальбумин мочи", "Моча"
		}
		if has("микроальбумин", "суточная моча", "креатинин мочи") {
			return "Биохимия мочи", "Моча"
		}
		return "Общий анализ мочи", "Моча"
	}
	if counts["biochemistry"] >= 1 || has("биохимический анализ крови") {
		return "Биохимия крови", "Кровь"
	}
	return "Лабораторное исследование", "Другие анализы"
}

// PresentAnalysis upgrades legacy records at the API boundary without altering
// the source transcription or the laboratory values saved in MongoDB.
func PresentAnalysis(value domain.Analysis) domain.Analysis {
	title, category := CanonicalAnalysisIdentity(value.Markers, value.OCRText, value.Report)
	value.Title, value.Category = title, category
	if value.Report != nil && (value.AIReview.Provider == "rules" || reviewLooksLikeReportRepeat(value.AIReview.Summary)) {
		value.AIReview = studyRuleReview(value.Report)
	}
	if value.Report == nil && len(value.Markers) > 0 && len(value.AIReview.Recommendations) == 0 {
		fallback := ruleReview(value.Markers)
		value.AIReview.Recommendations = fallback.Recommendations
		value.AIReview.RedFlags = fallback.RedFlags
		if isEnumerationOnly(value.AIReview.Summary) && fallback.Summary != "" {
			value.AIReview.Summary = fallback.Summary
		}
		if value.AIReview.SuggestedSpecialty == "" {
			value.AIReview.SuggestedSpecialty = fallback.SuggestedSpecialty
		}
	}
	return value
}

func reportValue(report *domain.StudyReport, get func(*domain.StudyReport) string) string {
	if report == nil {
		return ""
	}
	return get(report)
}

func normalizedAnalysisText(markers []domain.Marker, text string, report *domain.StudyReport) string {
	parts := []string{text}
	if report != nil {
		parts = append(parts, report.Modality, report.StudyName, report.Description, report.Conclusion)
	}
	for _, marker := range markers {
		parts = append(parts, marker.Name, marker.CanonicalName)
	}
	return normalizeSearch(strings.Join(parts, " "))
}

var nonSearchCharacter = regexp.MustCompile(`[^a-zа-я0-9%]+`)

func normalizeSearch(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "ё", "е"))
	return strings.TrimSpace(nonSearchCharacter.ReplaceAllString(value, " "))
}

func markerFamilies(markers []domain.Marker) map[string]int {
	result := map[string]int{}
	seen := map[string]bool{}
	for _, marker := range markers {
		key := normalizeSearch(marker.CanonicalName + " " + marker.Name)
		family := ""
		switch {
		case containsAny(key, "tsh", "thyroid stimulating", "тиреотроп", "free t3", "free t4", "трийодтиронин", "тироксин"):
			family = "thyroid"
		case containsAny(key, "urine", "microalbumin", "микроальбумин", "нитрит", "кетон", "уробилиноген", "относительная плотность", "бактерии", "слизь", "эпителий", "прозрачность", "цвет"):
			family = "urine"
		case containsAny(key, "wbc", "rbc", "hgb", "hct", "plt", "hemoglobin", "лейкоцит", "эритроцит", "гемоглобин", "гематокрит", "тромбоцит", "нейтрофил", "лимфоцит", "эозинофил", "моноцит", "gran percent", "lym percent", "соэ", "esr"):
			family = "cbc"
		case containsAny(key, "glucose", "creatinine", "urea", "cholesterol", "bilirubin", "albumin", "alt", "ast", "egfr", "gfr", "crp", "c reactive", "глюкоза", "креатинин", "мочевина", "холестерин", "билирубин", "альбумин", "кальций", "calcium", "железо", "iron"):
			family = "biochemistry"
		}
		identity := family + ":" + key
		if family != "" && !seen[identity] {
			result[family]++
			seen[identity] = true
		}
	}
	return result
}

func containsAny(value string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(value, normalizeSearch(part)) {
			return true
		}
	}
	return false
}

func reviewLooksLikeReportRepeat(summary string) bool {
	lower := normalizeSearch(summary)
	return strings.HasPrefix(lower, "в заключении исследования указано") || strings.HasPrefix(lower, "описание исследования сохранено")
}

func isEnumerationOnly(summary string) bool {
	lower := normalizeSearch(summary)
	return strings.Contains(lower, "остальные показатели") || strings.Contains(lower, "в анализе представлены показатели") || strings.Count(summary, ",") >= 5
}
