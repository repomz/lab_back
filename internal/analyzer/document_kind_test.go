package analyzer

import (
	"context"
	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
	"testing"
)

func TestNarrativeDocumentFolders(t *testing.T) {
	service := New(config.Config{})
	for _, tc := range []struct{ kind, name, modality, category string }{
		{"discharge", "Выписной эпикриз", "", "Выписки"},
		{"specialist", "Заключение кардиолога", "", "Заключения"},
		{"diagnostic", "Электрокардиография", "ЭКГ", "Другие исследования"},
		{"diagnostic", "УЗИ почек", "УЗИ", "УЗИ"},
	} {
		t.Run(tc.kind+tc.name, func(t *testing.T) {
			result, err := service.finishVisionExtraction(context.Background(), visionExtraction{DocumentType: tc.kind, Report: &domain.StudyReport{StudyName: tc.name, Modality: tc.modality, Description: "Подтверждённый текст документа. В анамнезе КТ не выполнялась."}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Report.DocumentKind != tc.kind || result.Category != tc.category || len(result.Markers) != 0 {
				t.Fatalf("wrong document routing: %+v", result)
			}
		})
	}
}

func TestUnknownInstrumentalStudyIsNotRenamedUltrasound(t *testing.T) {
	title, category := CanonicalAnalysisIdentity(nil, "", &domain.StudyReport{StudyName: "Спирометрия", DocumentKind: "diagnostic", Description: "Исследование функции дыхания"})
	if title != "Спирометрия" || category != "Другие исследования" {
		t.Fatalf("%s / %s", title, category)
	}
}

func TestRejectInvalidNarrativeKind(t *testing.T) {
	_, err := NormalizeConfirmedReport(&domain.StudyReport{DocumentKind: "invented", StudyName: "Исследование", Description: "Текст"})
	if err == nil {
		t.Fatal("unsupported document kind accepted")
	}
}
