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
	"github.com/repomz/lab_back/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type API struct {
	cfg       config.Config
	store     *store.Mongo
	analyzer  *analyzer.Service
	aiLimiter *aiUserLimiter
}
type actor struct {
	ID   primitive.ObjectID
	Role domain.Role
}
type contextKey string

const actorKey contextKey = "actor"

func New(cfg config.Config, s *store.Mongo, a *analyzer.Service) http.Handler {
	api := &API{cfg: cfg, store: s, analyzer: a, aiLimiter: newAIUserLimiter(cfg.AIUserRequestsPerMinute, cfg.AIUserRequestsPerHour)}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, api.cors)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/api/v1/articles/media/{name}", api.articleMedia)
	r.Post("/api/v1/auth/register", api.register)
	r.Post("/api/v1/auth/login", api.login)
	r.Group(func(r chi.Router) {
		r.Use(api.authorize)
		r.Get("/api/v1/me", api.me)
		r.Post("/api/v1/me/deletion-request", api.requestAccountDeletion)
		r.Delete("/api/v1/me/deletion-request", api.cancelAccountDeletion)
		r.Patch("/api/v1/me/contact-profile", api.updateContactProfile)
		r.Patch("/api/v1/me/patient-profile", api.updatePatientProfile)
		r.Patch("/api/v1/me/doctor-profile", api.updateDoctorProfile)
		r.Patch("/api/v1/me/settings", api.updateSettings)
		r.Post("/api/v1/me/avatar", api.uploadAvatar)
		r.Patch("/api/v1/me/avatar-preset", api.avatarPreset)
		r.Get("/api/v1/users/{id}/avatar", api.avatar)
		r.Get("/api/v1/doctors", api.doctors)
		r.Get("/api/v1/patients", api.patients)
		r.Get("/api/v1/admin/stats", api.appStats)
		r.Post("/api/v1/admin/impersonate", api.adminImpersonate)
		r.Get("/api/v1/analyses", api.analyses)
		r.With(api.limitAIRequests).Get("/api/v1/me/health-summary", api.healthSummary)
		r.Post("/api/v1/analyses", api.upload)
		r.Get("/api/v1/analyses/{id}", api.analysis)
		r.Get("/api/v1/analyses/{id}/file", api.file)
		r.Get("/api/v1/analyses/{id}/report.pdf", api.reportPDF)
		r.Delete("/api/v1/analyses/{id}", api.deleteAnalysis)
		r.Post("/api/v1/analyses/{id}/reprocess", api.reprocess)
		r.Post("/api/v1/analyses/{id}/confirm", api.confirmAnalysis)
		r.Post("/api/v1/analyses/{id}/share", api.share)
		r.Get("/api/v1/consultations", api.consultations)
		r.Post("/api/v1/consultations", api.createConsultation)
		r.With(api.limitAIRequests).Post("/api/v1/consultations/ai", api.createAIConsultation)
		r.Patch("/api/v1/consultations/{id}", api.reply)
		r.Post("/api/v1/consultations/{id}/messages", api.consultationMessage)
		r.Get("/api/v1/support/messages", api.supportMessages)
		r.Post("/api/v1/support/messages", api.createSupportMessage)
		r.With(api.limitAIRequests).Post("/api/v1/recommendations/{kind}", api.recommendation)
		r.With(api.limitAIRequests).Post("/api/v1/clinical-assist", api.clinicalAssist)
		r.Get("/api/v1/doctors/{id}/schedule", api.schedule)
		r.Put("/api/v1/doctor/schedule", api.replaceSchedule)
		r.Get("/api/v1/patients/{id}/notes", api.patientNotes)
		r.Post("/api/v1/patients/{id}/notes", api.createPatientNote)
		r.Get("/api/v1/ai/chats", api.aiChats)
		r.Post("/api/v1/ai/chats", api.createAIChat)
		r.Get("/api/v1/ai/chats/{id}", api.aiChat)
		r.Patch("/api/v1/ai/chats/{id}", api.renameAIChat)
		r.Delete("/api/v1/ai/chats/{id}", api.deleteAIChat)
		r.With(api.limitAIRequests).Post("/api/v1/ai/chats/{id}/messages", api.aiMessage)
		r.Get("/api/v1/articles", api.articleList)
		r.Get("/api/v1/articles/{id}", api.articleDetail)
		r.Post("/api/v1/articles", api.createArticle)
		r.Patch("/api/v1/articles/{id}", api.updateArticle)
		r.Delete("/api/v1/articles/{id}", api.deleteArticle)
		r.Post("/api/v1/articles/media", api.uploadArticleMedia)
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
		user, e := a.store.UserByID(r.Context(), id)
		if e != nil || user.Role != domain.Role(c.Role) || deletionExpired(user, time.Now().UTC()) {
			write(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := withActor(r.Context(), actor{id, user.Role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func withActor(ctx context.Context, v actor) context.Context {
	return context.WithValue(ctx, actorKey, v)
}
func current(r *http.Request) actor { return r.Context().Value(actorKey).(actor) }

func (a *API) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email, PIN, Role, FullName, BirthDate, Gender, Specialization, LicenseNumber string
		Age                                                                          int
		HeightCM, WeightKG                                                           float64
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.FullName = strings.TrimSpace(in.FullName)
	in.BirthDate = strings.TrimSpace(in.BirthDate)
	in.Gender = strings.ToLower(strings.TrimSpace(in.Gender))
	if in.FullName == "" {
		in.FullName = in.Email
	}
	role := domain.Role(in.Role)
	birth, birthErr := time.Parse("02.01.2006", in.BirthDate)
	now := time.Now()
	age := now.Year() - birth.Year()
	if now.Month() < birth.Month() || (now.Month() == birth.Month() && now.Day() < birth.Day()) {
		age--
	}
	patientValid := role == domain.RolePatient && birthErr == nil && age >= 18 && age <= 120 && (in.Gender == "female" || in.Gender == "male")
	doctorValid := role == domain.RoleDoctor && in.FullName != "" && strings.TrimSpace(in.Specialization) != ""
	if in.Email == "" || !validPIN(in.PIN) || (!patientValid && !doctorValid) {
		write(w, 422, map[string]string{"error": "проверьте роль, профиль и PIN из четырёх цифр"})
		return
	}
	h, e := auth.Hash(in.PIN)
	if e != nil {
		write(w, 500, map[string]string{"error": "registration failed"})
		return
	}
	u := domain.User{Email: in.Email, PasswordHash: h, Role: role, FullName: in.FullName, BirthDate: in.BirthDate, Gender: in.Gender, Specialization: strings.TrimSpace(in.Specialization), LicenseNumber: strings.TrimSpace(in.LicenseNumber), OnlineClinic: role == domain.RolePatient}
	if role == domain.RoleDoctor {
		u.DoctorProfile = &domain.DoctorProfile{ScheduleStep: 30, VisibleDays: 6}
	}
	// A new user only needs a name and a PIN. Health data is deliberately
	// collected later, after an explicit explanation of the personalisation it
	// enables. Keep compatibility with older clients that still send all three
	// measurements during registration.
	if role == domain.RolePatient && (in.Age != 0 || in.HeightCM != 0 || in.WeightKG != 0) {
		profile, profileErr := patientProfile(in.Age, in.HeightCM, in.WeightKG, "")
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
	_ = a.store.RecordUsageEvent(r.Context(), "login", u)
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
	if deletionExpired(u, time.Now().UTC()) {
		write(w, 410, map[string]string{"error": "профиль удалён"})
		return
	}
	token, _ := auth.Sign(a.cfg.JWTSecret, u.ID.Hex(), string(u.Role))
	_ = a.store.RecordUsageEvent(r.Context(), "login", u)
	write(w, 200, map[string]any{"token": token, "user": u})
}

func deletionGracePeriod(role domain.Role) time.Duration {
	if role == domain.RoleDoctor {
		return 3 * 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

func deletionExpired(user domain.User, now time.Time) bool {
	return user.DeletionScheduledFor != nil && !user.DeletionScheduledFor.After(now)
}

func (a *API) requestAccountDeletion(w http.ResponseWriter, r *http.Request) {
	u, err := a.store.UserByID(r.Context(), current(r).ID)
	if err != nil {
		write(w, 404, map[string]string{"error": "профиль не найден"})
		return
	}
	if u.Role == domain.RoleAdmin {
		write(w, 403, map[string]string{"error": "системный профиль администратора удалить нельзя"})
		return
	}
	if u.DeletionScheduledFor != nil {
		write(w, 200, u)
		return
	}
	now := time.Now().UTC()
	u, err = a.store.ScheduleAccountDeletion(r.Context(), u.ID, now, now.Add(deletionGracePeriod(u.Role)))
	if err != nil {
		write(w, 500, map[string]string{"error": "не удалось запланировать удаление"})
		return
	}
	write(w, 200, u)
}

func (a *API) cancelAccountDeletion(w http.ResponseWriter, r *http.Request) {
	u, err := a.store.CancelAccountDeletion(r.Context(), current(r).ID)
	if err != nil {
		write(w, 500, map[string]string{"error": "не удалось отменить удаление"})
		return
	}
	write(w, 200, u)
}

func (a *API) appStats(w http.ResponseWriter, r *http.Request) {
	if current(r).Role != domain.RoleAdmin {
		write(w, 403, map[string]string{"error": "only administrators can view application statistics"})
		return
	}
	stats, err := a.store.AppStats(r.Context(), time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		write(w, 500, map[string]string{"error": "could not load application statistics"})
		return
	}
	write(w, 200, stats)
}

func (a *API) adminImpersonate(w http.ResponseWriter, r *http.Request) {
	if current(r).Role != domain.RoleAdmin {
		write(w, 403, map[string]string{"error": "administrator access required"})
		return
	}
	var in struct{ Role string }
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	role := domain.Role(strings.TrimSpace(in.Role))
	if role != domain.RolePatient && role != domain.RoleDoctor {
		write(w, 422, map[string]string{"error": "choose patient or doctor"})
		return
	}
	u, err := a.store.FirstUserByRole(r.Context(), role)
	if err != nil {
		write(w, 404, map[string]string{"error": "no user is available for this role"})
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
func patientProfile(age int, heightCM, weightKG float64, birthDate string) (domain.PatientProfile, error) {
	birthDate = strings.TrimSpace(birthDate)
	if birthDate != "" {
		birth, err := time.Parse("02.01.2006", birthDate)
		if err != nil || birth.After(time.Now()) {
			return domain.PatientProfile{}, fmt.Errorf("укажите корректную дату рождения")
		}
		now := time.Now()
		age = now.Year() - birth.Year()
		if now.Month() < birth.Month() || (now.Month() == birth.Month() && now.Day() < birth.Day()) {
			age--
		}
	}
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
	return domain.PatientProfile{Age: age, BirthDate: birthDate, HeightCM: heightCM, WeightKG: weightKG, BMI: bmi}, nil
}
func (a *API) updatePatientProfile(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients have this profile"})
		return
	}
	var in struct {
		Age                int
		BirthDate          string
		HeightCM, WeightKG float64
		Activity           domain.ActivitySurvey
		Nutrition          domain.NutritionSurvey
		DevDataTTLHours    int
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	profile, err := patientProfile(in.Age, in.HeightCM, in.WeightKG, in.BirthDate)
	if err != nil {
		write(w, 422, map[string]string{"error": err.Error()})
		return
	}
	currentUser, _ := a.store.UserByID(r.Context(), u.ID)
	if currentUser.IsDeveloper && (in.DevDataTTLHours < 1 || in.DevDataTTLHours > 720) {
		write(w, 422, map[string]string{"error": "срок хранения тестовых данных должен быть от 1 до 720 часов"})
		return
	}
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
	if currentUser.IsDeveloper {
		updated, err = a.store.UpdateDeveloperTTL(r.Context(), u.ID, in.DevDataTTLHours)
		if err != nil {
			write(w, 500, map[string]string{"error": "could not update developer retention"})
			return
		}
	}
	write(w, 200, updated)
}
func (a *API) updateContactProfile(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	var in struct {
		FullName     string `json:"fullName"`
		ContactEmail string `json:"contactEmail"`
		Phone        string `json:"phone"`
		City         string `json:"city"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	in.FullName = strings.TrimSpace(in.FullName)
	in.ContactEmail = strings.TrimSpace(in.ContactEmail)
	in.Phone = strings.TrimSpace(in.Phone)
	in.City = strings.TrimSpace(in.City)
	if in.FullName == "" || len([]rune(in.FullName)) > 120 {
		write(w, 422, map[string]string{"error": "укажите имя длиной до 120 символов"})
		return
	}
	if len([]rune(in.ContactEmail)) > 254 || (in.ContactEmail != "" && !strings.Contains(in.ContactEmail, "@")) {
		write(w, 422, map[string]string{"error": "укажите корректную почту"})
		return
	}
	if len([]rune(in.Phone)) > 32 || len([]rune(in.City)) > 100 {
		write(w, 422, map[string]string{"error": "проверьте телефон и город"})
		return
	}
	updated, err := a.store.UpdateContactProfile(r.Context(), u.ID, in.FullName, in.ContactEmail, in.Phone, in.City)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not update contact profile"})
		return
	}
	write(w, 200, updated)
}

func (a *API) updateDoctorProfile(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors have this profile"})
		return
	}
	var in struct {
		FullName, Specialization, City, About, Workplace string
		Experience, Services                             []string
		ScheduleStep, VisibleDays                        int
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	in.FullName, in.Specialization, in.City = strings.TrimSpace(in.FullName), strings.TrimSpace(in.Specialization), strings.TrimSpace(in.City)
	if in.FullName == "" || in.Specialization == "" || len([]rune(in.FullName)) > 120 || len([]rune(in.Specialization)) > 100 || len(in.Experience) > 20 || len(in.Services) > 20 {
		write(w, 422, map[string]string{"error": "проверьте ФИО, специальность и разделы профиля"})
		return
	}
	if in.ScheduleStep != 15 && in.ScheduleStep != 20 && in.ScheduleStep != 30 && in.ScheduleStep != 60 {
		in.ScheduleStep = 30
	}
	if in.VisibleDays < 3 || in.VisibleDays > 7 {
		in.VisibleDays = 6
	}
	profile := domain.DoctorProfile{About: strings.TrimSpace(in.About), Workplace: strings.TrimSpace(in.Workplace), Experience: cleanLines(in.Experience), Services: cleanLines(in.Services), ScheduleStep: in.ScheduleStep, VisibleDays: in.VisibleDays}
	updated, err := a.store.UpdateDoctorProfile(r.Context(), u.ID, in.FullName, in.Specialization, in.City, profile)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not update doctor profile"})
		return
	}
	write(w, 200, updated)
}

func cleanLines(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && len([]rune(value)) <= 500 {
			out = append(out, value)
		}
	}
	return out
}

func (a *API) updateSettings(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only users can change this setting"})
		return
	}
	var in struct{ OnlineClinic bool }
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	updated, err := a.store.UpdateOnlineClinic(r.Context(), u.ID, in.OnlineClinic)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not update settings"})
		return
	}
	write(w, 200, updated)
}
func (a *API) doctors(w http.ResponseWriter, r *http.Request) {
	list, e := a.store.Doctors(r.Context(), r.URL.Query().Get("specialty"), r.URL.Query().Get("city"))
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
	if len([]rune(in.Objective))+len([]rune(in.Clinical)) > 12000 {
		write(w, 422, map[string]string{"error": "clinical data is too long"})
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
		writeAIServiceError(w, err, 502, "clinical assistant is temporarily unavailable")
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

func (a *API) healthSummary(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can view their health summary"})
		return
	}
	list, err := a.store.AnalysesFor(r.Context(), u.ID, u.Role)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not load analyses"})
		return
	}
	patient, err := a.store.UserByID(r.Context(), u.ID)
	if err != nil {
		write(w, 404, map[string]string{"error": "patient not found"})
		return
	}
	result := a.analyzer.PatientHealthSummary(r.Context(), patient, list)
	write(w, 200, result)
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
	text, markers, status := a.analyzer.RecognizeForPatient(r.Context(), path, mime, patient.PatientProfile)
	category := analyzer.ClassifyAnalysis(markers, text)
	item := domain.Analysis{OwnerID: u.ID, Title: category, Category: category, CollectedAt: analyzer.ExtractCollectedAt(text), OriginalName: filepath.Base(header.Filename), MimeType: mime, StoragePath: path, OCRText: text, Markers: markers, AIReview: domain.AIReview{}, Status: status, SharedWith: []primitive.ObjectID{}, IsDeveloper: patient.IsDeveloper}
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
	text, markers, status := a.analyzer.RecognizeForPatient(r.Context(), item.StoragePath, item.MimeType, patient.PatientProfile)
	collectedAt := analyzer.ExtractCollectedAt(text)
	if err = a.store.UpdateAnalysisRecognition(r.Context(), id, u.ID, text, markers, domain.AIReview{}, status, collectedAt); err != nil {
		write(w, 500, map[string]string{"error": "could not update recognition"})
		return
	}
	item.OCRText, item.Markers, item.AIReview, item.Status, item.CollectedAt = text, markers, domain.AIReview{}, status, collectedAt
	write(w, 200, item)
}

func (a *API) confirmAnalysis(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can confirm analyses"})
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
	if item.Status == "ready" {
		write(w, 200, item)
		return
	}
	var in struct {
		Markers []domain.Marker `json:"markers"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	markers, err := analyzer.NormalizeConfirmedMarkers(in.Markers)
	if err != nil {
		write(w, 422, map[string]string{"error": "check marker values"})
		return
	}
	patient, _ := a.store.UserByID(r.Context(), u.ID)
	review := a.analyzer.ReviewMarkersForPatient(r.Context(), markers, patient.PatientProfile)
	if err = a.store.ConfirmAnalysis(r.Context(), id, u.ID, markers, review); err != nil {
		write(w, 500, map[string]string{"error": "could not confirm analysis"})
		return
	}
	item.Markers, item.AIReview, item.Status = markers, review, "ready"
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
	if u.Role == domain.RoleDoctor {
		for i := range list {
			if patient, err := a.store.UserByID(r.Context(), list[i].PatientID); err == nil {
				list[i].PatientName = patient.FullName
			}
		}
	}
	write(w, 200, list)
}
func (a *API) supportMessages(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	var list []domain.SupportMessage
	var err error
	if u.Role == domain.RoleAdmin {
		list, err = a.store.AllSupportMessages(r.Context())
		for i := range list {
			if patient, userErr := a.store.UserByID(r.Context(), list[i].UserID); userErr == nil {
				list[i].PatientName = patient.FullName
			}
		}
	} else {
		list, err = a.store.SupportMessages(r.Context(), u.ID)
	}
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
	var in struct {
		Text   string `json:"text"`
		UserID string `json:"userId"`
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
	target, sender := u.ID, "patient"
	if u.Role == domain.RoleAdmin {
		parsed, parseErr := primitive.ObjectIDFromHex(strings.TrimSpace(in.UserID))
		if parseErr != nil {
			write(w, 422, map[string]string{"error": "patient is required"})
			return
		}
		target, sender = parsed, "support"
	}
	message := domain.SupportMessage{UserID: target, Sender: sender, Text: in.Text}
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
	if c.Question != "" {
		c.Messages = []domain.ConsultationMessage{{Sender: "patient", Text: c.Question, CreatedAt: time.Now().UTC()}}
	}
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
		writeAIServiceError(w, err, 503, "ИИ-консультация временно недоступна")
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

func (a *API) consultationMessage(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient && u.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "chat is not available"})
		return
	}
	id, err := parseID(chi.URLParam(r, "id"))
	var in struct{ Text string }
	if err != nil || decode(r, &in) != nil || strings.TrimSpace(in.Text) == "" || len([]rune(in.Text)) > 4000 {
		write(w, 422, map[string]string{"error": "message is required"})
		return
	}
	message := domain.ConsultationMessage{Sender: string(u.Role), Text: strings.TrimSpace(in.Text), CreatedAt: time.Now().UTC()}
	if err = a.store.AppendConsultationMessage(r.Context(), id, u.ID, u.Role, message); err != nil {
		write(w, 404, map[string]string{"error": "consultation not found"})
		return
	}
	write(w, 201, message)
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
		From, To    string
		Starts      []string
		SlotMinutes int
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
		if e != nil || v.Before(from) || !v.Before(to) || v.Before(time.Now().UTC()) {
			write(w, 422, map[string]string{"error": "invalid slot"})
			return
		}
		if !seen[v.Unix()] {
			seen[v.Unix()] = true
			starts = append(starts, v.UTC())
		}
	}
	if err = a.store.ReplaceSchedule(r.Context(), u.ID, from.UTC(), to.UTC(), starts, in.SlotMinutes); err != nil {
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
func (a *API) requireAIUser(w http.ResponseWriter, r *http.Request) (actor, bool) {
	u := current(r)
	if u.Role != domain.RoleDoctor && u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "AI workspace is not available for this role"})
		return u, false
	}
	return u, true
}
func (a *API) aiChats(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireAIUser(w, r)
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
	u, ok := a.requireAIUser(w, r)
	if !ok {
		return
	}
	var in struct{ Title string }
	_ = decode(r, &in)
	title := strings.TrimSpace(in.Title)
	if len([]rune(title)) > 120 {
		write(w, 422, map[string]string{"error": "title is too long"})
		return
	}
	if title == "" {
		title = time.Now().Format("02.01.2006 · 15:04")
	}
	chat := domain.AIChat{DoctorID: u.ID, Title: title}
	if e := a.store.CreateAIChat(r.Context(), &chat); e != nil {
		write(w, 500, map[string]string{"error": "could not create chat"})
		return
	}
	write(w, 201, chat)
}
func (a *API) aiChat(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireAIUser(w, r)
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
	u, ok := a.requireAIUser(w, r)
	if !ok {
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	var in struct{ Title string }
	if e != nil || decode(r, &in) != nil || strings.TrimSpace(in.Title) == "" || len([]rune(strings.TrimSpace(in.Title))) > 120 {
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
	u, ok := a.requireAIUser(w, r)
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
	u, ok := a.requireAIUser(w, r)
	if !ok {
		return
	}
	id, e := parseID(chi.URLParam(r, "id"))
	var in struct{ Content string }
	if e != nil || decode(r, &in) != nil || strings.TrimSpace(in.Content) == "" || len([]rune(strings.TrimSpace(in.Content))) > 4000 {
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
		writeAIServiceError(w, e, 503, "AI temporarily unavailable")
		return
	}
	assistant := domain.AIMessage{Role: "assistant", Content: reply, CreatedAt: time.Now().UTC()}
	if e = a.store.AppendAIChat(r.Context(), id, u.ID, userMessage, assistant); e != nil {
		write(w, 500, map[string]string{"error": "could not save messages"})
		return
	}
	write(w, 201, assistant)
}
func (a *API) articleList(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.ClinicalArticles(r.Context(), current(r).Role == domain.RoleDoctor)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not load articles"})
		return
	}
	if items == nil {
		items = []domain.ClinicalArticle{}
	}
	write(w, 200, items)
}

func (a *API) articleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid article id"})
		return
	}
	item, err := a.store.ClinicalArticle(r.Context(), id, current(r).Role == domain.RoleDoctor)
	if err != nil {
		write(w, 404, map[string]string{"error": "article not found"})
		return
	}
	write(w, 200, item)
}

func articleInput(r *http.Request) (domain.ClinicalArticle, error) {
	var in domain.ClinicalArticle
	if err := decode(r, &in); err != nil {
		return in, err
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Summary = strings.TrimSpace(in.Summary)
	in.CoverURL = strings.TrimSpace(in.CoverURL)
	if in.Title == "" || len([]rune(in.Title)) > 180 || len(in.Blocks) > 30 {
		return in, fmt.Errorf("invalid article")
	}
	for i := range in.Blocks {
		if in.Blocks[i].ID == "" {
			in.Blocks[i].ID = primitive.NewObjectID().Hex()
		}
		if in.Blocks[i].Type != "text" && in.Blocks[i].Type != "image" {
			return in, fmt.Errorf("invalid block")
		}
		in.Blocks[i].Text = strings.TrimSpace(in.Blocks[i].Text)
		in.Blocks[i].ImageURL = strings.TrimSpace(in.Blocks[i].ImageURL)
		in.Blocks[i].Caption = strings.TrimSpace(in.Blocks[i].Caption)
	}
	return in, nil
}

func (a *API) createArticle(w http.ResponseWriter, r *http.Request) {
	actor := current(r)
	if actor.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can publish articles"})
		return
	}
	item, err := articleInput(r)
	if err != nil {
		write(w, 400, map[string]string{"error": "check article fields"})
		return
	}
	item.ID = primitive.NilObjectID
	item.DoctorID = actor.ID
	if err = a.store.SaveClinicalArticle(r.Context(), &item); err != nil {
		write(w, 500, map[string]string{"error": "could not save article"})
		return
	}
	write(w, 201, item)
}

func (a *API) updateArticle(w http.ResponseWriter, r *http.Request) {
	actor := current(r)
	if actor.Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can edit articles"})
		return
	}
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid article id"})
		return
	}
	item, err := articleInput(r)
	if err != nil {
		write(w, 400, map[string]string{"error": "check article fields"})
		return
	}
	item.ID = id
	item.DoctorID = actor.ID
	if err = a.store.SaveClinicalArticle(r.Context(), &item); err != nil {
		write(w, 404, map[string]string{"error": "article not found"})
		return
	}
	write(w, 200, item)
}

func (a *API) deleteArticle(w http.ResponseWriter, r *http.Request) {
	if current(r).Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can delete articles"})
		return
	}
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid article id"})
		return
	}
	if err = a.store.DeleteClinicalArticle(r.Context(), id); err != nil {
		write(w, 404, map[string]string{"error": "article not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) uploadArticleMedia(w http.ResponseWriter, r *http.Request) {
	if current(r).Role != domain.RoleDoctor {
		write(w, 403, map[string]string{"error": "only doctors can upload article images"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxUploadMB<<20)
	if err := r.ParseMultipartForm(a.cfg.MaxUploadMB << 20); err != nil {
		write(w, 400, map[string]string{"error": "image is too large"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		write(w, 400, map[string]string{"error": "image is required"})
		return
	}
	defer file.Close()
	buffer := make([]byte, 512)
	n, _ := file.Read(buffer)
	mime := http.DetectContentType(buffer[:n])
	if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" {
		write(w, 400, map[string]string{"error": "JPEG, PNG or WebP required"})
		return
	}
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}[mime]
	dir := filepath.Join(a.cfg.UploadDir, "article-media")
	if err = os.MkdirAll(dir, 0o750); err != nil {
		write(w, 500, map[string]string{"error": "could not prepare storage"})
		return
	}
	name := primitive.NewObjectID().Hex() + ext
	path := filepath.Join(dir, name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not save image"})
		return
	}
	_, writeErr := out.Write(buffer[:n])
	if writeErr == nil {
		_, writeErr = io.Copy(out, file)
	}
	closeErr := out.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		write(w, 500, map[string]string{"error": "could not save image"})
		return
	}
	_ = header
	write(w, 201, map[string]string{"url": "/api/v1/articles/media/" + name})
}

func (a *API) articleMedia(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(chi.URLParam(r, "name"))
	if name == "." || name == "" || name != chi.URLParam(r, "name") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(a.cfg.UploadDir, "article-media", name)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}
