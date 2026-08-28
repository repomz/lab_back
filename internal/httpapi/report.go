package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-pdf/fpdf"
	"github.com/repomz/lab_back/internal/domain"
)

func (a *API) reportPDF(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	item, err := a.store.Analysis(r.Context(), id)
	if err != nil || !canRead(item, current(r)) {
		write(w, http.StatusNotFound, map[string]string{"error": "analysis not found"})
		return
	}
	payload, err := buildAnalysisPDF(item)
	if err != nil {
		write(w, http.StatusInternalServerError, map[string]string{"error": "could not create report"})
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", "analysis-"+id.Hex()+".pdf"))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	_, _ = w.Write(payload)
}

func buildAnalysisPDF(item domain.Analysis) ([]byte, error) {
	regular, bold, err := reportFonts()
	if err != nil {
		return nil, err
	}
	pdf := fpdf.New("P", "mm", "A4", filepath.ToSlash(filepath.Dir(regular)))
	pdf.SetMargins(16, 15, 16)
	pdf.SetAutoPageBreak(true, 18)
	pdf.SetTitle(item.Title, true)
	pdf.SetAuthor("Lab", true)
	pdf.AddUTF8Font("Lab", "", filepath.Base(regular))
	pdf.AddUTF8Font("Lab", "B", filepath.Base(bold))
	pdf.AddPage()

	pdf.SetFillColor(30, 49, 93)
	pdf.Rect(0, 0, 210, 32, "F")
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Lab", "B", 19)
	pdf.SetXY(16, 10)
	pdf.CellFormat(178, 8, "Результаты лабораторного анализа", "", 1, "L", false, 0, "")
	pdf.SetFont("Lab", "", 10)
	pdf.SetX(16)
	pdf.CellFormat(178, 6, "Сформировано приложением Lab", "", 1, "L", false, 0, "")

	pdf.SetTextColor(28, 35, 48)
	pdf.SetY(41)
	pdf.SetFont("Lab", "B", 15)
	pdf.MultiCell(178, 7, item.Title, "", "L", false)
	pdf.SetFont("Lab", "", 9)
	pdf.SetTextColor(92, 101, 118)
	pdf.CellFormat(178, 6, "Дата загрузки: "+item.CreatedAt.Local().Format("02.01.2006 15:04"), "", 1, "L", false, 0, "")
	if item.CollectedAt != nil {
		pdf.CellFormat(178, 6, "Дата исследования: "+item.CollectedAt.Local().Format("02.01.2006"), "", 1, "L", false, 0, "")
	}
	pdf.Ln(4)

	widths := []float64{66, 35, 45, 32}
	headers := []string{"Показатель", "Результат", "Референс", "Статус"}
	pdf.SetFillColor(235, 241, 249)
	pdf.SetDrawColor(210, 218, 230)
	pdf.SetTextColor(28, 35, 48)
	pdf.SetFont("Lab", "B", 9)
	for i, header := range headers {
		pdf.CellFormat(widths[i], 9, header, "1", 0, "L", true, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Lab", "", 9)
	for _, marker := range item.Markers {
		if pdf.GetY() > 267 {
			pdf.AddPage()
		}
		value := marker.TextValue
		if marker.Value != nil {
			value = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", *marker.Value), "0"), ".")
		}
		if marker.Unit != "" {
			value += " " + marker.Unit
		}
		reference := marker.ReferenceText
		if reference == "" {
			switch {
			case marker.ReferenceMin != nil && marker.ReferenceMax != nil:
				reference = fmt.Sprintf("%g — %g", *marker.ReferenceMin, *marker.ReferenceMax)
			case marker.ReferenceMin != nil:
				reference = fmt.Sprintf("от %g", *marker.ReferenceMin)
			case marker.ReferenceMax != nil:
				reference = fmt.Sprintf("до %g", *marker.ReferenceMax)
			default:
				reference = "Не указан"
			}
		}
		cells := []string{shortPDFText(marker.Name, 42), shortPDFText(value, 24), shortPDFText(reference, 28), markerStatusLabel(marker.Status)}
		for i, cell := range cells {
			pdf.CellFormat(widths[i], 9, cell, "1", 0, "L", false, 0, "")
		}
		pdf.Ln(-1)
	}
	if len(item.Markers) == 0 {
		pdf.CellFormat(178, 12, "Показатели не распознаны", "1", 1, "C", false, 0, "")
	}
	pdf.Ln(7)
	pdf.SetFont("Lab", "", 8)
	pdf.SetTextColor(92, 101, 118)
	pdf.MultiCell(178, 5, "Документ содержит автоматически распознанные данные. Сверяйте значения с оригинальным бланком и обсуждайте медицинские решения с врачом.", "", "L", false)

	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func reportFonts() (string, string, error) {
	pairs := [][2]string{
		{"/usr/share/fonts/dejavu/DejaVuSans.ttf", "/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf"},
		{"/usr/share/fonts/ttf-dejavu/DejaVuSans.ttf", "/usr/share/fonts/ttf-dejavu/DejaVuSans-Bold.ttf"},
		{"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"},
		{"/Library/Fonts/Arial Unicode.ttf", "/Library/Fonts/Arial Unicode.ttf"},
	}
	for _, pair := range pairs {
		if _, err := os.Stat(pair[0]); err != nil {
			continue
		}
		if _, err := os.Stat(pair[1]); err == nil {
			return filepath.Clean(pair[0]), filepath.Clean(pair[1]), nil
		}
	}
	return "", "", fmt.Errorf("unicode report font not found")
}

func shortPDFText(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes-1]) + "…"
}

func markerStatusLabel(status domain.MarkerStatus) string {
	switch status {
	case domain.StatusHigh:
		return "Выше нормы"
	case domain.StatusLow:
		return "Ниже нормы"
	case domain.StatusNormal:
		return "Норма"
	default:
		return "Проверить"
	}
}
