package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
	cfg      config.Config
	store    *store.Mongo
	analyzer *analyzer.Service
}
type actor struct {
	ID   primitive.ObjectID
	Role domain.Role
}
type contextKey string

const actorKey contextKey = "actor"

func New(cfg config.Config, s *store.Mongo, a *analyzer.Service) http.Handler {
	api := &API{cfg, s, a}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, api.cors)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	r.Post("/api/v1/auth/register", api.register)
	r.Post("/api/v1/auth/login", api.login)
	r.Group(func(r chi.Router) {
		r.Use(api.authorize)
		r.Get("/api/v1/me", api.me)
		r.Get("/api/v1/doctors", api.doctors)
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
		r.Patch("/api/v1/consultations/{id}", api.reply)
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
	var in struct{ Email, Password, Role, FullName, Specialization, LicenseNumber string }
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
	if in.Email == "" || in.Password == "" || (role != domain.RolePatient && role != domain.RoleDoctor) {
		write(w, 422, map[string]string{"error": "логин, пароль и роль обязательны"})
		return
	}
	if role == domain.RoleDoctor && strings.TrimSpace(in.Specialization) == "" {
		write(w, 422, map[string]string{"error": "specialization is required for doctors"})
		return
	}
	h, e := auth.Hash(in.Password)
	if e != nil {
		write(w, 500, map[string]string{"error": "registration failed"})
		return
	}
	u := domain.User{Email: in.Email, PasswordHash: h, Role: role, FullName: in.FullName, Specialization: strings.TrimSpace(in.Specialization), LicenseNumber: strings.TrimSpace(in.LicenseNumber)}
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
	var in struct{ Email, Password string }
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	u, e := a.store.UserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(in.Email)))
	if e != nil || !auth.Verify(u.PasswordHash, in.Password) {
		write(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	token, _ := auth.Sign(a.cfg.JWTSecret, u.ID.Hex(), string(u.Role))
	write(w, 200, map[string]any{"token": token, "user": u})
}
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	u, e := a.store.UserByID(r.Context(), current(r).ID)
	if e != nil {
		write(w, 404, map[string]string{"error": "user not found"})
		return
	}
	write(w, 200, u)
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
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(header.Filename), ext)
	}
	text, markers, review, status := a.analyzer.Process(r.Context(), path, mime)
	item := domain.Analysis{OwnerID: u.ID, Title: title, OriginalName: filepath.Base(header.Filename), MimeType: mime, StoragePath: path, OCRText: text, Markers: markers, AIReview: review, Status: status, SharedWith: []primitive.ObjectID{}}
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
	text, markers, review, status := a.analyzer.Process(r.Context(), item.StoragePath, item.MimeType)
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
func (a *API) createConsultation(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Role != domain.RolePatient {
		write(w, 403, map[string]string{"error": "only patients can request consultations"})
		return
	}
	var in struct{ AnalysisID, DoctorID, Question string }
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	aid, e1 := parseID(in.AnalysisID)
	did, e2 := parseID(in.DoctorID)
	if e1 != nil || e2 != nil {
		write(w, 422, map[string]string{"error": "invalid analysis or doctor id"})
		return
	}
	item, e := a.store.Analysis(r.Context(), aid)
	if e != nil || item.OwnerID != u.ID {
		write(w, 404, map[string]string{"error": "analysis not found"})
		return
	}
	doctor, e := a.store.UserByID(r.Context(), did)
	if e != nil || doctor.Role != domain.RoleDoctor {
		write(w, 404, map[string]string{"error": "doctor not found"})
		return
	}
	if e = a.store.Share(r.Context(), aid, u.ID, did); e != nil {
		write(w, 500, map[string]string{"error": "could not grant access"})
		return
	}
	c := domain.Consultation{AnalysisID: aid, PatientID: u.ID, DoctorID: did, Question: strings.TrimSpace(in.Question)}
	if e = a.store.CreateConsultation(r.Context(), &c); e != nil {
		write(w, 500, map[string]string{"error": "could not request consultation"})
		return
	}
	write(w, 201, c)
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
