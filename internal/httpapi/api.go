package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/repomz/lab_back/internal/analyzer"
	"github.com/repomz/lab_back/internal/auth"
	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/domain"
	"github.com/repomz/lab_back/internal/guides"
	"github.com/repomz/lab_back/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type API struct {
	cfg      config.Config
	store    *store.Mongo
	analyzer *analyzer.Service
	guides   *guides.Service
}
type actor struct {
	ID   primitive.ObjectID
	Role domain.Role
}
type contextKey string

const actorKey contextKey = "actor"

func New(cfg config.Config, s *store.Mongo, a *analyzer.Service) http.Handler {
	api := &API{cfg: cfg, store: s, analyzer: a, guides: guides.New()}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, api.cors)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	r.Post("/api/v1/auth/register", api.register)
	r.Post("/api/v1/auth/login", api.login)
	r.Group(func(r chi.Router) {
		r.Use(api.authorize)
		r.Get("/api/v1/me", api.me)
		r.Patch("/api/v1/me/contact-profile", api.updateContactProfile)
		r.Patch("/api/v1/me/patient-profile", api.updatePatientProfile)
		r.Post("/api/v1/me/avatar", api.uploadAvatar)
		r.Patch("/api/v1/me/avatar-preset", api.avatarPreset)
		r.Get("/api/v1/users/{id}/avatar", api.avatar)
		r.Get("/api/v1/doctors", api.doctors)
		r.Get("/api/v1/patients", api.patients)
		r.Get("/api/v1/analyses", api.analyses)
		r.Post("/api/v1/analyses", api.upload)
		r.Get("/api/v1/analyses/{id}", api.analysis)
		r.Get("/api/v1/analyses/{id}/file", api.file)
		r.Get("/api/v1/analyses/{id}/report.pdf", api.reportPDF)
		r.Delete("/api/v1/analyses/{id}", api.deleteAnalysis)
		r.Post("/api/v1/analyses/{id}/reprocess", api.reprocess)
		r.Post("/api/v1/analyses/{id}/share", api.share)
		r.Get("/api/v1/consultations", api.consultations)
		r.Post("/api/v1/consultations", api.createConsultation)
		r.Post("/api/v1/consultations/ai", api.createAIConsultation)
		r.Patch("/api/v1/consultations/{id}", api.reply)
		r.Get("/api/v1/support/messages", api.supportMessages)
		r.Post("/api/v1/support/messages", api.createSupportMessage)
		r.Post("/api/v1/recommendations/{kind}", api.recommendation)
		r.Post("/api/v1/clinical-assist", api.clinicalAssist)
		r.Get("/api/v1/doctors/{id}/schedule", api.schedule)
		r.Put("/api/v1/doctor/schedule", api.replaceSchedule)
		r.Get("/api/v1/patients/{id}/notes", api.patientNotes)
		r.Post("/api/v1/patients/{id}/notes", api.createPatientNote)
		r.Get("/api/v1/ai/chats", api.aiChats)
		r.Post("/api/v1/ai/chats", api.createAIChat)
		r.Get("/api/v1/ai/chats/{id}", api.aiChat)
		r.Patch("/api/v1/ai/chats/{id}", api.renameAIChat)
		r.Delete("/api/v1/ai/chats/{id}", api.deleteAIChat)
		r.Post("/api/v1/ai/chats/{id}/messages", api.aiMessage)
		r.Get("/api/v1/guides", api.guideList)
		r.Get("/api/v1/guides/{id}", api.guideDetail)
		r.Post("/api/v1/guides/sync", api.syncGuides)
	})
	return r
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, allowed := range a.cfg.CORSOrigins {
			if origin == strings.TrimSpace(allowed) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
		}
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *API) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" {
			raw = r.URL.Query().Get("access_token")
		}
		c, e := auth.Parse(a.cfg.JWTSecret, raw)
		if e != nil {
			write(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		id, e := primitive.ObjectIDFromHex(c.UserID)
		if e != nil {
			write(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := r.Context()
		ctx = withActor(ctx, actor{id, domain.Role(c.Role)})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func withActor(ctx context.Context, v actor) context.Context {
	return context.WithValue(ctx, actorKey, v)
}
func current(r *http.Request) actor { return r.Context().Value(actorKey).(actor) }

func (a *API) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email, PIN, Role, FullName, Specialization, LicenseNumber string
		Age                                                       int
		HeightCM, WeightKG                                        float64
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.FullName = strings.TrimSpace(in.FullName)
	if in.FullName == "" {
		in.FullName = in.Email
	}
	role := domain.Role(in.Role)
	if in.Email == "" || !validPIN(in.PIN) || role != domain.RolePatient {
		write(w, 422, map[string]string{"error": "регистрация доступна только пользователям; нужны логин и PIN из четырёх цифр"})
		return
	}
	h, e := auth.Hash(in.PIN)
	if e != nil {
		write(w, 500, map[string]string{"error": "registration failed"})
		return
	}
	u := domain.User{Email: in.Email, PasswordHash: h, Role: role, FullName: in.FullName, Specialization: strings.TrimSpace(in.Specialization), LicenseNumber: strings.TrimSpace(in.LicenseNumber)}
	if role == domain.RolePatient {
		profile, profileErr := patientProfile(in.Age, in.HeightCM, in.WeightKG)
		if profileErr != nil {
			write(w, 422, map[string]string{"error": profileErr.Error()})
			return
		}
		u.PatientProfile = &profile
	}
	if e = a.store.CreateUser(r.Context(), &u); e != nil {
		if mongo.IsDuplicateKeyError(e) {
			write(w, 409, map[string]string{"error": "такой логин уже занят"})
			return
		}
		write(w, 500, map[string]string{"error": "registration failed"})
		return
	}
	token, _ := auth.Sign(a.cfg.JWTSecret, u.ID.Hex(), string(u.Role))
	write(w, 201, map[string]any{"token": token, "user": u})
}
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, PIN string }
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(in.Email) == "" || !validPIN(in.PIN) {
		write(w, 422, map[string]string{"error": "введите логин и PIN из четырёх цифр"})
		return
	}
	u, e := a.store.UserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(in.Email)))
	if e != nil || !auth.Verify(u.PasswordHash, in.PIN) {
		write(w, 401, map[string]string{"error": "неверный логин или PIN"})
		return
	}
	token, _ := auth.Sign(a.cfg.JWTSecret, u.ID.Hex(), string(u.Role))
	write(w, 200, map[string]any{"token": token, "user": u})
}
func validPIN(value string) bool {
	if len(value) != 4 {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	u, e := a.store.UserByID(r.Context(), current(r).ID)
	if e != nil {
		write(w, 404, map[string]string{"error": "user not found"})
		return
	}
	write(w, 200, u)
}
func (a *API) uploadAvatar(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if e := r.ParseMultipartForm(6 << 20); e != nil {
		write(w, 413, map[string]string{"error": "фото должно быть меньше 5 МБ"})
		return
	}
	file, _, e := r.FormFile("file")
	if e != nil {
		write(w, 422, map[string]string{"error": "выберите фотографию"})
		return
	}
	defer file.Close()
	data, e := io.ReadAll(io.LimitReader(file, (5<<20)+1))
	if e != nil || len(data) > 5<<20 {
		write(w, 413, map[string]string{"error": "фото должно быть меньше 5 МБ"})
		return
	}
	mime := http.DetectContentType(data)
	ext := ""
	switch mime {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	default:
		write(w, 415, map[string]string{"error": "поддерживаются JPG, PNG и WebP"})
		return
	}
	dir := filepath.Join(a.cfg.UploadDir, "avatars")
	if e = os.MkdirAll(dir, 0700); e != nil {
		write(w, 500, map[string]string{"error": "storage unavailable"})
		return
	}
	path := filepath.Join(dir, u.ID.Hex()+ext)
	if e = os.WriteFile(path, data, 0600); e != nil {
		write(w, 500, map[string]string{"error": "could not save avatar"})
		return
	}
	previous, _ := a.store.UserByID(r.Context(), u.ID)
	updated, e := a.store.UpdateAvatar(r.Context(), u.ID, path, "")
	if e != nil {
		_ = os.Remove(path)
		write(w, 500, map[string]string{"error": "could not save avatar"})
		return
	}
	if previous.AvatarPath != "" && previous.AvatarPath != path && strings.HasPrefix(previous.AvatarPath, dir+string(os.PathSeparator)) {
		_ = os.Remove(previous.AvatarPath)
	}
	write(w, 200, updated)
}
func (a *API) avatarPreset(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "для врача доступна только фотография"})
		return
	}
	var in struct{ Preset string }
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	allowed := map[string]bool{"person": true, "leaf": true, "heart": true, "sun": true}
	if !allowed[in.Preset] {
		write(w, 422, map[string]string{"error": "unknown avatar"})
		return
	}
	previous, _ := a.store.UserByID(r.Context(), u.ID)
	updated, e := a.store.UpdateAvatar(r.Context(), u.ID, "", in.Preset)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not save avatar"})
		return
	}
	if previous.AvatarPath != "" {
		_ = os.Remove(previous.AvatarPath)
	}
	write(w, 200, updated)
}
func (a *API) avatar(w http.ResponseWriter, r *http.Request) {
	id, e := parseID(chi.URLParam(r, "id"))
	if e != nil {
		http.NotFound(w, r)
		return
	}
	user, e := a.store.UserByID(r.Context(), id)
	if e != nil || user.AvatarPath == "" {
		http.NotFound(w, r)
		return
	}
	file, e := os.Open(user.AvatarPath)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, "avatar"+filepath.Ext(user.AvatarPath), info.ModTime(), file)
}
func patientProfile(age int, heightCM, weightKG float64) (domain.PatientProfile, error) {
	if age < 1 || age > 120 {
		return domain.PatientProfile{}, fmt.Errorf("укажите возраст от 1 до 120 лет")
	}
	if heightCM < 50 || heightCM > 250 {
		return domain.PatientProfile{}, fmt.Errorf("укажите корректный рост в сантиметрах")
	}
	if weightKG < 5 || weightKG > 400 {
		return domain.PatientProfile{}, fmt.Errorf("укажите корректный вес в килограммах")
	}
	heightM := heightCM / 100
	bmi := math.Round(weightKG/(heightM*heightM)*10) / 10
	return domain.PatientProfile{Age: age, HeightCM: heightCM, WeightKG: weightKG, BMI: bmi}, nil
}
func (a *API) updatePatientProfile(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients have this profile"})
		return
	}
	var in struct {
		Age                int
		HeightCM, WeightKG float64
		Activity           domain.ActivitySurvey
		Nutrition          domain.NutritionSurvey
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	profile, err := patientProfile(in.Age, in.HeightCM, in.WeightKG)
	if err != nil {
		write(w, 422, map[string]string{"error": err.Error()})
		return
	}
	currentUser, _ := a.store.UserByID(r.Context(), u.ID)
	if currentUser.PatientProfile != nil {
		profile.ActivityRecommendation = currentUser.PatientProfile.ActivityRecommendation
		profile.NutritionRecommendation = currentUser.PatientProfile.NutritionRecommendation
	}
	profile.Activity = in.Activity
	profile.Nutrition = in.Nutrition
	updated, err := a.store.UpdatePatientProfile(r.Context(), u.ID, profile)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not update profile"})
		return
	}
	write(w, 200, updated)
}
func (a *API) updateContactProfile(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	var in struct {
		FullName           string `json:"fullName"`
		Phone              string `json:"phone"`
		ResidentialAddress string `json:"residentialAddress"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	in.FullName = strings.TrimSpace(in.FullName)
	in.Phone = strings.TrimSpace(in.Phone)
	in.ResidentialAddress = strings.TrimSpace(in.ResidentialAddress)
	if in.FullName == "" || len([]rune(in.FullName)) > 120 {
		write(w, 422, map[string]string{"error": "укажите имя длиной до 120 символов"})
		return
	}
	if len([]rune(in.Phone)) > 32 || len([]rune(in.ResidentialAddress)) > 300 {
		write(w, 422, map[string]string{"error": "проверьте телефон и адрес"})
		return
	}
	updated, err := a.store.UpdateContactProfile(r.Context(), u.ID, in.FullName, in.Phone, in.ResidentialAddress)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not update contact profile"})
		return
	}
	write(w, 200, updated)
}
func (a *API) doctors(w http.ResponseWriter, r *http.Request) {
	list, e := a.store.Doctors(r.Context(), r.URL.Query().Get("specialty"))
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load doctors"})
		return
	}
	if list == nil {
		list = []domain.User{}
	}
	write(w, 200, list)
}
func (a *API) patients(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can view patients"})
		return
	}
	list, err := a.store.PatientsForDoctor(r.Context(), u.ID)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not load patients"})
		return
	}
	write(w, 200, list)
}
func (a *API) clinicalAssist(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can use clinical assist"})
		return
	}
	var in struct {
		PatientID string `json:"patient_id"`
		Objective string `json:"objective"`
		Clinical  string `json:"clinical"`
	}
	if decode(r, &in) != nil || (strings.TrimSpace(in.Objective) == "" && strings.TrimSpace(in.Clinical) == "") {
		write(w, 422, map[string]string{"error": "patient and clinical data are required"})
		return
	}
	patientID, err := parseID(in.PatientID)
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid patient id"})
		return
	}
	analyses, err := a.store.SharedAnalysesForPatient(r.Context(), u.ID, patientID)
	if err != nil || len(analyses) == 0 {
		write(w, 403, map[string]string{"error": "patient has not shared analyses with this doctor"})
		return
	}
	patient, err := a.store.UserByID(r.Context(), patientID)
	if err != nil || patient.Role != domain.RolePatient {
		write(w, 404, map[string]string{"error": "patient not found"})
		return
	}
	result, err := a.analyzer.ClinicalAssist(r.Context(), patient, in.Objective, in.Clinical, analyses)
	if err != nil {
		write(w, 502, map[string]string{"error": "clinical assistant is temporarily unavailable"})
		return
	}
	write(w, 200, result)
}
func (a *API) analyses(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	list, e := a.store.AnalysesFor(r.Context(), u.ID, u.Role)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load analyses"})
		return
	}
	if list == nil {
		list = []domain.Analysis{}
	}
	write(w, 200, list)
}
func (a *API) upload(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can upload analyses"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxUploadMB<<20)
	if e := r.ParseMultipartForm(a.cfg.MaxUploadMB << 20); e != nil {
		write(w, 413, map[string]string{"error": fmt.Sprintf("file must be smaller than %d MB", a.cfg.MaxUploadMB)})
		return
	}
	file, header, e := r.FormFile("file")
	if e != nil {
		write(w, 422, map[string]string{"error": "file is required"})
		return
	}
	defer file.Close()
	mime := header.Header.Get("Content-Type")
	if !(strings.HasPrefix(mime, "image/") || mime == "application/pdf") {
		write(w, 415, map[string]string{"error": "only image and PDF files are supported"})
		return
	}
	id := primitive.NewObjectID()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = ".bin"
	}
	dir := filepath.Join(a.cfg.UploadDir, u.ID.Hex())
	if e = os.MkdirAll(dir, 0700); e != nil {
		write(w, 500, map[string]string{"error": "storage unavailable"})
		return
	}
	path := filepath.Join(dir, id.Hex()+ext)
	dst, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		write(w, 500, map[string]string{"error": "storage unavailable"})
		return
	}
	_, copyErr := io.Copy(dst, file)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		write(w, 500, map[string]string{"error": "upload failed"})
		return
	}
	patient, _ := a.store.UserByID(r.Context(), u.ID)
	text, markers, review, status := a.analyzer.ProcessForPatient(r.Context(), path, mime, patient.PatientProfile)
	category := analyzer.ClassifyAnalysis(markers, text)
	item := domain.Analysis{OwnerID: u.ID, Title: category, Category: category, OriginalName: filepath.Base(header.Filename), MimeType: mime, StoragePath: path, OCRText: text, Markers: markers, AIReview: review, Status: status, SharedWith: []primitive.ObjectID{}}
	item.ID = id
	if e = a.store.CreateAnalysis(r.Context(), &item); e != nil {
		_ = os.Remove(path)
		write(w, 500, map[string]string{"error": "could not save analysis"})
		return
	}
	write(w, 201, item)
}
func parseID(raw string) (primitive.ObjectID, error) { return primitive.ObjectIDFromHex(raw) }
func canRead(a domain.Analysis, u actor) bool {
	if a.OwnerID == u.ID {
		return true
	}
	for _, id := range a.SharedWith {
		if id == u.ID {
			return true
		}
	}
	return false
}
func (a *API) analysis(w http.ResponseWriter, r *http.Request) {
	id, e := parseID(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	item, e := a.store.Analysis(r.Context(), id)
	if e != nil {
		write(w, 404, map[string]string{"error": "analysis not found"})
		return
	}
	if !canRead(item, current(r)) {
		write(w, 403, map[string]string{"error": "access denied"})
		return
	}
	write(w, 200, item)
}
func (a *API) file(w http.ResponseWriter, r *http.Request) {
	id, e := parseID(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	item, e := a.store.Analysis(r.Context(), id)
	if e != nil || !canRead(item, current(r)) {
		write(w, 404, map[string]string{"error": "file not found"})
		return
	}
	w.Header().Set("Content-Type", item.MimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", item.OriginalName))
	http.ServeFile(w, r, item.StoragePath)
}
func (a *API) deleteAnalysis(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can delete analyses"})
		return
	}
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	item, err := a.store.Analysis(r.Context(), id)
	if err != nil || item.OwnerID != u.ID {
		write(w, 404, map[string]string{"error": "analysis not found"})
		return
	}
	storagePath := filepath.Clean(item.StoragePath)
	uploadRoot := filepath.Clean(a.cfg.UploadDir)
	if storagePath == uploadRoot || !strings.HasPrefix(storagePath, uploadRoot+string(os.PathSeparator)) {
		write(w, 500, map[string]string{"error": "invalid storage path"})
		return
	}
	trashPath := storagePath + ".deleting-" + id.Hex()
	fileMoved := false
	if err = os.Rename(storagePath, trashPath); err == nil {
		fileMoved = true
	} else if !os.IsNotExist(err) {
		write(w, 500, map[string]string{"error": "could not remove original file"})
		return
	}
	if err = a.store.DeleteAnalysis(r.Context(), id, u.ID); err != nil {
		if fileMoved {
			_ = os.Rename(trashPath, storagePath)
		}
		write(w, 500, map[string]string{"error": "could not delete analysis"})
		return
	}
	if fileMoved {
		_ = os.Remove(trashPath)
	}
	_ = os.Remove(filepath.Dir(storagePath)) // succeeds only when the owner directory is empty
	w.WriteHeader(http.StatusNoContent)
}
func (a *API) reprocess(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can reprocess analyses"})
		return
	}
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	item, err := a.store.Analysis(r.Context(), id)
	if err != nil || item.OwnerID != u.ID {
		write(w, 404, map[string]string{"error": "analysis not found"})
		return
	}
	if _, err = os.Stat(item.StoragePath); err != nil {
		write(w, 404, map[string]string{"error": "original file not found"})
		return
	}
	patient, _ := a.store.UserByID(r.Context(), u.ID)
	text, markers, review, status := a.analyzer.ProcessForPatient(r.Context(), item.StoragePath, item.MimeType, patient.PatientProfile)
	if err = a.store.UpdateAnalysisRecognition(r.Context(), id, u.ID, text, markers, review, status); err != nil {
		write(w, 500, map[string]string{"error": "could not update recognition"})
		return
	}
	item.OCRText, item.Markers, item.AIReview, item.Status = text, markers, review, status
	write(w, 200, item)
}
func (a *API) share(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can share analyses"})
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid analysis id"})
		return
	}
	var in struct {
		DoctorID string `json:"doctor_id"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	doctor, e := parseID(in.DoctorID)
	if e != nil {
		write(w, 422, map[string]string{"error": "invalid doctor id"})
		return
	}
	d, e := a.store.UserByID(r.Context(), doctor)
	if e != nil || d.Role != domain.RoleDoctor {
		write(w, 404, map[string]string{"error": "doctor not found"})
		return
	}
	if e = a.store.Share(r.Context(), id, u.ID, doctor); e != nil {
		write(w, 404, map[string]string{"error": "analysis not found"})
		return
	}
	write(w, 200, map[string]string{"status": "shared"})
}
func (a *API) consultations(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	list, e := a.store.Consultations(r.Context(), u.ID, u.Role)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load consultations"})
		return
	}
	if list == nil {
		list = []domain.Consultation{}
	}
	write(w, 200, list)
}
func (a *API) supportMessages(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "support chat is available to patients"})
		return
	}
	list, err := a.store.SupportMessages(r.Context(), u.ID)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not load support chat"})
		return
	}
	if list == nil {
		list = []domain.SupportMessage{}
	}
	write(w, 200, list)
}
func (a *API) createSupportMessage(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "support chat is available to patients"})
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" || len([]rune(in.Text)) > 4000 {
		write(w, 422, map[string]string{"error": "message must contain 1 to 4000 characters"})
		return
	}
	message := domain.SupportMessage{UserID: u.ID, Sender: "patient", Text: in.Text}
	if err := a.store.CreateSupportMessage(r.Context(), &message); err != nil {
		write(w, 500, map[string]string{"error": "could not send support message"})
		return
	}
	write(w, 201, message)
}
func (a *API) createConsultation(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can request consultations"})
		return
	}
	var in struct {
		AnalysisID, DoctorID, Question, ServiceType, AppointmentAt string
		PersonalDataConsent                                        bool `json:"personalDataConsent"`
		MedicalDataConsent                                         bool `json:"medicalDataConsent"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	did, e2 := parseID(in.DoctorID)
	if e2 != nil {
		write(w, 422, map[string]string{"error": "invalid doctor id"})
		return
	}
	var aid primitive.ObjectID
	if strings.TrimSpace(in.AnalysisID) != "" {
		var parseErr error
		aid, parseErr = parseID(in.AnalysisID)
		if parseErr != nil {
			write(w, 422, map[string]string{"error": "invalid analysis id"})
			return
		}
		item, lookupErr := a.store.Analysis(r.Context(), aid)
		if lookupErr != nil || item.OwnerID != u.ID {
			write(w, 404, map[string]string{"error": "analysis not found"})
			return
		}
	}
	doctor, e := a.store.UserByID(r.Context(), did)
	if e != nil || doctor.Role != domain.RoleDoctor {
		write(w, 404, map[string]string{"error": "doctor not found"})
		return
	}
	if !aid.IsZero() {
		if e = a.store.Share(r.Context(), aid, u.ID, did); e != nil {
			write(w, 500, map[string]string{"error": "could not grant access"})
			return
		}
	}
	if in.MedicalDataConsent {
		if e = a.store.ShareAllAnalyses(r.Context(), u.ID, did); e != nil {
			write(w, 500, map[string]string{"error": "could not grant access to examinations"})
			return
		}
	}
	serviceType := strings.TrimSpace(in.ServiceType)
	if serviceType == "" {
		serviceType = "consultation"
	}
	title := "Консультация врача"
	if serviceType == "appointment" {
		title = "Запись на приём"
	}
	if serviceType == "home_visit" {
		if !doctor.HomeVisits {
			write(w, 422, map[string]string{"error": "врач не выполняет вызовы на дом"})
			return
		}
		title = "Вызов врача на дом"
	}
	var appointmentAt *time.Time
	if in.AppointmentAt != "" {
		parsed, parseErr := time.Parse(time.RFC3339, in.AppointmentAt)
		if parseErr != nil {
			write(w, 422, map[string]string{"error": "invalid appointment time"})
			return
		}
		appointmentAt = &parsed
	}
	if serviceType == "appointment" && appointmentAt == nil {
		write(w, 422, map[string]string{"error": "выберите время приёма"})
		return
	}
	c := domain.Consultation{ID: primitive.NewObjectID(), AnalysisID: aid, PatientID: u.ID, DoctorID: did, Source: "doctor", Title: title, Specialty: doctor.Specialization, ServiceType: serviceType, AppointmentAt: appointmentAt, PersonalDataConsent: in.PersonalDataConsent, MedicalDataConsent: in.MedicalDataConsent, Question: strings.TrimSpace(in.Question)}
	if serviceType == "appointment" {
		if e = a.store.ReserveSlot(r.Context(), did, u.ID, c.ID, appointmentAt.UTC()); e != nil {
			write(w, 409, map[string]string{"error": "это время уже занято; выберите другое"})
			return
		}
	}
	if e = a.store.CreateConsultation(r.Context(), &c); e != nil {
		if serviceType == "appointment" {
			a.store.ReleaseSlot(r.Context(), c.ID)
		}
		write(w, 500, map[string]string{"error": "could not request consultation"})
		return
	}
	write(w, 201, c)
}
func (a *API) createAIConsultation(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can use ai consultations"})
		return
	}
	var in struct{ Question string }
	if decode(r, &in) != nil || strings.TrimSpace(in.Question) == "" {
		write(w, 422, map[string]string{"error": "опишите, что вас беспокоит"})
		return
	}
	if len([]rune(in.Question)) > 4000 {
		write(w, 422, map[string]string{"error": "сообщение слишком длинное"})
		return
	}
	patient, err := a.store.UserByID(r.Context(), u.ID)
	if err != nil {
		write(w, 404, map[string]string{"error": "patient not found"})
		return
	}
	analyses, _ := a.store.AnalysesFor(r.Context(), u.ID, u.Role)
	result, err := a.analyzer.SymptomConsultation(r.Context(), patient.PatientProfile, in.Question, analyses)
	if err != nil {
		write(w, 503, map[string]string{"error": "ИИ-консультация временно недоступна"})
		return
	}
	c := domain.Consultation{PatientID: u.ID, Source: "ai", Title: result.Title, Specialty: result.Specialty, Question: strings.TrimSpace(in.Question), Reply: result.Answer, Status: "answered"}
	if !result.Accepted {
		c.Title = "Сообщение не относится к здоровью"
		c.Specialty = ""
	}
	if err = a.store.CreateConsultation(r.Context(), &c); err != nil {
		write(w, 500, map[string]string{"error": "could not save consultation"})
		return
	}
	write(w, 201, c)
}
func (a *API) recommendation(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can request recommendations"})
		return
	}
	kind := chi.URLParam(r, "kind")
	if kind != "activity" && kind != "nutrition" {
		write(w, 404, map[string]string{"error": "unknown recommendation type"})
		return
	}
	var in struct {
		Activity  domain.ActivitySurvey
		Nutrition domain.NutritionSurvey
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	patient, err := a.store.UserByID(r.Context(), u.ID)
	if err != nil || patient.PatientProfile == nil {
		write(w, 422, map[string]string{"error": "сначала заполните возраст, рост и вес в профиле"})
		return
	}
	profile := *patient.PatientProfile
	if kind == "activity" {
		profile.Activity = in.Activity
	} else {
		profile.Nutrition = in.Nutrition
	}
	analyses, _ := a.store.AnalysesFor(r.Context(), u.ID, u.Role)
	recommendation, _ := a.analyzer.Recommendation(r.Context(), kind, profile, analyses)
	if kind == "activity" {
		profile.ActivityRecommendation = recommendation
	} else {
		profile.NutritionRecommendation = recommendation
	}
	updated, err := a.store.UpdatePatientProfile(r.Context(), u.ID, profile)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not save recommendation"})
		return
	}
	write(w, 200, map[string]any{"recommendation": recommendation, "user": updated})
}
func (a *API) reply(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can reply"})
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	var in struct{ Reply, Status string }
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if in.Status == "" {
		in.Status = "answered"
	}
	if e = a.store.Reply(r.Context(), id, u.ID, strings.TrimSpace(in.Reply), in.Status); e != nil {
		write(w, 404, map[string]string{"error": "consultation not found"})
		return
	}
	write(w, 200, map[string]string{"status": in.Status})
}

func dateRange(r *http.Request) (time.Time, time.Time, error) {
	from, err := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err != nil || !to.After(from) || to.Sub(from) > 32*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid date range")
	}
	return from.UTC(), to.UTC(), nil
}
func (a *API) schedule(w http.ResponseWriter, r *http.Request) {
	doctor, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid doctor id"})
		return
	}
	from, to, err := dateRange(r)
	if err != nil {
		write(w, 422, map[string]string{"error": "invalid date range"})
		return
	}
	items, err := a.store.Schedule(r.Context(), doctor, from, to)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not load schedule"})
		return
	}
	if current(r).Role != domain.RoleDoctor || current(r).ID != doctor {
		for i := range items {
			items[i].PatientID = primitive.NilObjectID
			items[i].AppointmentID = primitive.NilObjectID
			items[i].PatientName = ""
		}
	}
	if items == nil {
		items = []domain.ScheduleSlot{}
	}
	write(w, 200, items)
}
func (a *API) replaceSchedule(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can edit schedule"})
		return
	}
	var in struct {
		From, To string
		Starts   []string
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	from, err := time.Parse(time.RFC3339, in.From)
	if err != nil {
		write(w, 422, map[string]string{"error": "invalid from"})
		return
	}
	to, err := time.Parse(time.RFC3339, in.To)
	if err != nil || !to.After(from) || to.Sub(from) > 8*24*time.Hour {
		write(w, 422, map[string]string{"error": "invalid week range"})
		return
	}
	starts := make([]time.Time, 0, len(in.Starts))
	seen := map[int64]bool{}
	for _, raw := range in.Starts {
		v, e := time.Parse(time.RFC3339, raw)
		if e != nil || v.Before(from) || !v.Before(to) || v.Before(time.Now().UTC()) || v.Minute()%30 != 0 {
			write(w, 422, map[string]string{"error": "invalid slot"})
			return
		}
		if !seen[v.Unix()] {
			seen[v.Unix()] = true
			starts = append(starts, v.UTC())
		}
	}
	if err = a.store.ReplaceSchedule(r.Context(), u.ID, from.UTC(), to.UTC(), starts); err != nil {
		write(w, 500, map[string]string{"error": "could not save schedule"})
		return
	}
	write(w, 200, map[string]string{"status": "saved"})
}
func (a *API) patientNotes(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can view notes"})
		return
	}
	pid, e := parseID(chi.URLParam(r, "id"))
	if e != nil || !a.store.PatientAccessible(r.Context(), u.ID, pid) {
		write(w, 403, map[string]string{"error": "patient is not available"})
		return
	}
	items, e := a.store.PatientNotes(r.Context(), u.ID, pid)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load notes"})
		return
	}
	if items == nil {
		items = []domain.PatientNote{}
	}
	write(w, 200, items)
}
func (a *API) createPatientNote(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can add notes"})
		return
	}
	pid, e := parseID(chi.URLParam(r, "id"))
	if e != nil || !a.store.PatientAccessible(r.Context(), u.ID, pid) {
		write(w, 403, map[string]string{"error": "patient is not available"})
		return
	}
	var in struct{ Text string }
	if decode(r, &in) != nil || strings.TrimSpace(in.Text) == "" {
		write(w, 422, map[string]string{"error": "заключение не может быть пустым"})
		return
	}
	if len([]rune(in.Text)) > 12000 {
		write(w, 422, map[string]string{"error": "заключение слишком длинное"})
		return
	}
	note := domain.PatientNote{DoctorID: u.ID, PatientID: pid, Text: strings.TrimSpace(in.Text)}
	if e = a.store.CreatePatientNote(r.Context(), &note); e != nil {
		write(w, 500, map[string]string{"error": "could not save note"})
		return
	}
	write(w, 201, note)
}
func (a *API) requireDoctor(w http.ResponseWriter, r *http.Request) (actor, bool) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can use AI workspace"})
		return u, false
	}
	return u, true
}
func (a *API) aiChats(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireDoctor(w, r)
	if !ok {
		return
	}
	items, e := a.store.AIChats(r.Context(), u.ID)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load chats"})
		return
	}
	if items == nil {
		items = []domain.AIChat{}
	}
	write(w, 200, items)
}
func (a *API) createAIChat(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireDoctor(w, r)
	if !ok {
		return
	}
	var in struct{ Title string }
	_ = decode(r, &in)
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Консультация · " + time.Now().Format("02.01.2006 15:04")
	}
	chat := domain.AIChat{DoctorID: u.ID, Title: title}
	if e := a.store.CreateAIChat(r.Context(), &chat); e != nil {
		write(w, 500, map[string]string{"error": "could not create chat"})
		return
	}
	write(w, 201, chat)
}
func (a *API) aiChat(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireDoctor(w, r)
	if !ok {
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid chat id"})
		return
	}
	chat, e := a.store.AIChat(r.Context(), id, u.ID)
	if e != nil {
		write(w, 404, map[string]string{"error": "chat not found"})
		return
	}
	write(w, 200, chat)
}
func (a *API) renameAIChat(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireDoctor(w, r)
	if !ok {
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	var in struct{ Title string }
	if e != nil || decode(r, &in) != nil || strings.TrimSpace(in.Title) == "" {
		write(w, 422, map[string]string{"error": "invalid title"})
		return
	}
	if e = a.store.RenameAIChat(r.Context(), id, u.ID, strings.TrimSpace(in.Title)); e != nil {
		write(w, 404, map[string]string{"error": "chat not found"})
		return
	}
	write(w, 200, map[string]string{"status": "saved"})
}
func (a *API) deleteAIChat(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireDoctor(w, r)
	if !ok {
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	if e != nil || a.store.DeleteAIChat(r.Context(), id, u.ID) != nil {
		write(w, 404, map[string]string{"error": "chat not found"})
		return
	}
	w.WriteHeader(204)
}
func (a *API) aiMessage(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireDoctor(w, r)
	if !ok {
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	var in struct{ Content string }
	if e != nil || decode(r, &in) != nil || strings.TrimSpace(in.Content) == "" {
		write(w, 422, map[string]string{"error": "message is required"})
		return
	}
	chat, e := a.store.AIChat(r.Context(), id, u.ID)
	if e != nil {
		write(w, 404, map[string]string{"error": "chat not found"})
		return
	}
	now := time.Now().UTC()
	userMessage := domain.AIMessage{Role: "user", Content: strings.TrimSpace(in.Content), CreatedAt: now}
	history := append(chat.Messages, userMessage)
	reply, e := a.analyzer.DoctorChat(r.Context(), history)
	if e != nil {
		write(w, 503, map[string]string{"error": "AI temporarily unavailable"})
		return
	}
	assistant := domain.AIMessage{Role: "assistant", Content: reply, CreatedAt: time.Now().UTC()}
	if e = a.store.AppendAIChat(r.Context(), id, u.ID, userMessage, assistant); e != nil {
		write(w, 500, map[string]string{"error": "could not save messages"})
		return
	}
	write(w, 201, assistant)
}
func (a *API) guideList(w http.ResponseWriter, r *http.Request) {
	if current(r).Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can view guides"})
		return
	}
	items, synced, e := a.guides.List(r.Context())
	if e != nil {
		write(w, 503, map[string]string{"error": "official catalog is temporarily unavailable"})
		return
	}
	write(w, 200, map[string]any{"items": items, "synced_at": synced, "source": "Минздрав России"})
}
func (a *API) guideDetail(w http.ResponseWriter, r *http.Request) {
	if current(r).Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can view guides"})
		return
	}
	item, e := a.guides.Get(r.Context(), chi.URLParam(r, "id"))
	if e != nil {
		write(w, 502, map[string]string{"error": "could not load the official guide"})
		return
	}
	write(w, 200, item)
}
func (a *API) syncGuides(w http.ResponseWriter, r *http.Request) {
	if current(r).Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can sync guides"})
		return
	}
	if e := a.guides.Sync(r.Context()); e != nil {
		write(w, 503, map[string]string{"error": "official catalog is temporarily unavailable"})
		return
	}
	items, synced, _ := a.guides.List(r.Context())
	write(w, 200, map[string]any{"items": items, "synced_at": synced, "source": "Минздрав России"})
}
