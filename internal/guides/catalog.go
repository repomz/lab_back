package guides

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/repomz/lab_back/internal/domain"
)

const apiURL = "https://apicr.minzdrav.gov.ru/api.ashx"

var safeID = regexp.MustCompile(`^[0-9]+_[0-9]+$`)
var tags = regexp.MustCompile(`<[^>]+>`)
var spaces = regexp.MustCompile(`[ \t\r\f\v]+`)

type Service struct {
	mu      sync.RWMutex
	items   []domain.Guide
	synced  time.Time
	details map[string]domain.Guide
	client  *http.Client
}

func New() *Service {
	return &Service{client: &http.Client{Timeout: 30 * time.Second}, details: map[string]domain.Guide{}}
}

func (s *Service) request(ctx context.Context, method, operation string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL+"?op="+operation, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LabHealth/0.4; +https://135.106.195.235)")
	req.Header.Set("Referer", "https://cr.minzdrav.gov.ru/")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minzdrav API status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 24<<20))
}
func (s *Service) List(ctx context.Context) ([]domain.Guide, time.Time, error) {
	s.mu.RLock()
	fresh := len(s.items) > 0 && time.Since(s.synced) < 24*time.Hour
	s.mu.RUnlock()
	if !fresh {
		if err := s.Sync(ctx); err != nil {
			s.mu.RLock()
			defer s.mu.RUnlock()
			if len(s.items) == 0 {
				return nil, time.Time{}, err
			}
			return append([]domain.Guide(nil), s.items...), s.synced, nil
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.Guide(nil), s.items...), s.synced, nil
}
func (s *Service) Sync(ctx context.Context) error {
	payload, _ := json.Marshal(map[string]any{"filters": []any{}, "sortOption": map[string]any{"fieldName": "publishdate", "sortType": 2}, "pageSize": 2500, "currentPage": 1, "useANDoperator": true, "columns": []any{}})
	raw, err := s.request(ctx, http.MethodPost, "GetJsonClinrecsFilterV2", payload)
	if err != nil {
		return err
	}
	var envelope struct {
		Data []struct {
			Name, CodeVersion, PublishDateStr, AgeCategoryStr string
			ApplyStatusCalculated                             int
			Mkbs                                              []struct{ MkbCode string }
			Developers                                        []struct{ NkoShortName string }
		}
		TotalRecords int
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("empty minzdrav catalog")
	}
	items := make([]domain.Guide, 0, len(envelope.Data))
	now := time.Now().UTC()
	for _, v := range envelope.Data {
		codes := make([]string, 0, len(v.Mkbs))
		for _, m := range v.Mkbs {
			codes = append(codes, m.MkbCode)
		}
		devs := make([]string, 0, len(v.Developers))
		for _, d := range v.Developers {
			devs = append(devs, d.NkoShortName)
		}
		published, _ := time.Parse("2006-01-02T15:04:05", v.PublishDateStr)
		status := "Применяется"
		if v.ApplyStatusCalculated == 2 {
			status = "Применение отложено"
		} else if v.ApplyStatusCalculated == 3 {
			status = "Применяется предыдущая редакция"
		}
		items = append(items, domain.Guide{ID: v.CodeVersion, Code: strings.Join(codes, ", "), Title: v.Name, Category: v.AgeCategoryStr, Status: status, Developers: devs, PublishedAt: published, SourceURL: "https://cr.minzdrav.gov.ru/preview-cr/" + v.CodeVersion, UpdatedAt: now})
	}
	s.mu.Lock()
	s.items = items
	s.synced = now
	s.mu.Unlock()
	return nil
}
func cleanHTML(value string) string {
	value = strings.ReplaceAll(value, "<br>", "\n")
	value = strings.ReplaceAll(value, "<br/>", "\n")
	value = strings.ReplaceAll(value, "</p>", "\n")
	value = strings.ReplaceAll(value, "</li>", "\n")
	value = tags.ReplaceAllString(value, "")
	value = html.UnescapeString(value)
	lines := strings.Split(value, "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(spaces.ReplaceAllString(line, " "))
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
func (s *Service) Get(ctx context.Context, id string) (domain.Guide, error) {
	if !safeID.MatchString(id) {
		return domain.Guide{}, fmt.Errorf("invalid guide id")
	}
	s.mu.RLock()
	if item, ok := s.details[id]; ok {
		s.mu.RUnlock()
		return item, nil
	}
	s.mu.RUnlock()
	raw, err := s.request(ctx, http.MethodGet, "GetClinrec2&id="+id, nil)
	if err != nil {
		return domain.Guide{}, err
	}
	var doc struct {
		Name, ID, MKB, Created string
		Obj                    struct {
			Sections []struct {
				ID, Title, Content string
				Data               []struct{ Title, Content string }
			}
		}
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		return domain.Guide{}, err
	}
	guide := domain.Guide{ID: id, Title: doc.Name, Code: doc.MKB, SourceURL: "https://cr.minzdrav.gov.ru/preview-cr/" + id, UpdatedAt: time.Now().UTC()}
	for _, section := range doc.Obj.Sections {
		text := cleanHTML(section.Content)
		if text == "" && len(section.Data) > 0 {
			parts := []string{}
			for _, data := range section.Data {
				content := cleanHTML(data.Content)
				if content != "" {
					parts = append(parts, strings.TrimSpace(data.Title+": "+content))
				}
			}
			text = strings.Join(parts, "\n")
		}
		title := strings.TrimSpace(section.Title)
		if title != "" && text != "" && section.ID != "doc_whole" {
			guide.Sections = append(guide.Sections, domain.GuideSection{ID: section.ID, Title: title, Content: text})
		}
	}
	if len(guide.Sections) == 0 {
		return domain.Guide{}, fmt.Errorf("guide has no readable sections")
	}
	s.mu.Lock()
	s.details[id] = guide
	s.mu.Unlock()
	return guide, nil
}
