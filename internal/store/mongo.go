package store

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/repomz/lab_back/internal/domain"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Mongo struct{ db *mongo.Database }

func Connect(ctx context.Context, uri, database string) (*Mongo, error) {
	c, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err = c.Ping(ctx, nil); err != nil {
		return nil, err
	}
	s := &Mongo{db: c.Database(database)}
	indexes := []struct {
		collection string
		models     []mongo.IndexModel
	}{
		{"users", []mongo.IndexModel{
			{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true)},
			{Keys: bson.D{{Key: "deletion_scheduled_for", Value: 1}}},
		}},
		{"schedule_slots", []mongo.IndexModel{{Keys: bson.D{{Key: "doctor_id", Value: 1}, {Key: "start_at", Value: 1}}, Options: options.Index().SetUnique(true)}}},
		{"analyses", []mongo.IndexModel{
			{Keys: bson.D{{Key: "owner_id", Value: 1}, {Key: "created_at", Value: -1}}},
			{Keys: bson.D{{Key: "shared_with", Value: 1}, {Key: "created_at", Value: -1}}},
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "processing_next_attempt_at", Value: 1}, {Key: "created_at", Value: 1}}},
		}},
		{"usage_events", []mongo.IndexModel{{Keys: bson.D{{Key: "kind", Value: 1}, {Key: "created_at", Value: -1}}}}},
		{"consultations", []mongo.IndexModel{
			{Keys: bson.D{{Key: "patient_id", Value: 1}, {Key: "created_at", Value: -1}}},
			{Keys: bson.D{{Key: "doctor_id", Value: 1}, {Key: "created_at", Value: -1}}},
		}},
		{"support_messages", []mongo.IndexModel{{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: 1}}}}},
		{"ai_chats", []mongo.IndexModel{{Keys: bson.D{{Key: "doctor_id", Value: 1}, {Key: "updated_at", Value: -1}}}}},
		{"patient_notes", []mongo.IndexModel{{Keys: bson.D{{Key: "patient_id", Value: 1}, {Key: "created_at", Value: -1}}}}},
		{"clinical_articles", []mongo.IndexModel{{Keys: bson.D{{Key: "published", Value: -1}, {Key: "updated_at", Value: -1}}}}},
	}
	for _, group := range indexes {
		if _, err = s.db.Collection(group.collection).Indexes().CreateMany(ctx, group.models); err != nil {
			return nil, err
		}
	}
	var seedMarker bson.M
	seedErr := s.db.Collection("app_settings").FindOne(ctx, bson.M{"_id": "clinical_articles_seed_v1"}).Decode(&seedMarker)
	if errors.Is(seedErr, mongo.ErrNoDocuments) {
		seedID, _ := primitive.ObjectIDFromHex("66d000000000000000000001")
		now := time.Now().UTC()
		_, err = s.db.Collection("clinical_articles").UpdateOne(ctx, bson.M{"_id": seedID}, bson.M{"$setOnInsert": domain.ClinicalArticle{
			ID: seedID, Title: "Сонные артерии: как атеросклероз влияет на кровоснабжение мозга",
			Summary:  "Наглядный разбор формирования бляшки, стеноза сонной артерии и методов визуальной диагностики.",
			CoverURL: "/clinical-carotid-overview.svg", Published: true, CreatedAt: now, UpdatedAt: now,
			Blocks: []domain.ArticleBlock{
				{ID: "intro", Type: "text", Text: "Сонные артерии доставляют кровь к головному мозгу. Атеросклеротическая бляшка чаще формируется в области бифуркации общей сонной артерии и постепенно сужает её просвет."},
				{ID: "image", Type: "image", ImageURL: "/clinical-carotid-overview.svg", Caption: "Схема стеноза и ангиографическое представление сонной артерии."},
				{ID: "symptoms", Type: "text", Text: "Транзиторная слабость в руке или ноге, асимметрия лица, внезапное нарушение речи или зрения требуют срочной медицинской оценки. Даже кратковременные симптомы могут быть проявлением транзиторной ишемической атаки."},
				{ID: "diagnostics", Type: "text", Text: "Для первичной оценки обычно применяют ультразвуковое дуплексное сканирование. КТ-ангиография, МР-ангиография или рентгеноконтрастная ангиография помогают уточнить анатомию и степень сужения. Тактика всегда определяется врачом индивидуально."},
			},
		}}, options.Update().SetUpsert(true))
		if err != nil {
			return nil, err
		}
		_, err = s.db.Collection("app_settings").InsertOne(ctx, bson.M{"_id": "clinical_articles_seed_v1", "created_at": now})
		if err != nil && !mongo.IsDuplicateKeyError(err) {
			return nil, err
		}
	} else if seedErr != nil {
		return nil, seedErr
	}
	return s, nil
}
func (s *Mongo) CreateUser(ctx context.Context, u *domain.User) error {
	u.ID = primitive.NewObjectID()
	u.CreatedAt = time.Now().UTC()
	_, err := s.db.Collection("users").InsertOne(ctx, u)
	return err
}
func (s *Mongo) EnsureSystemUser(ctx context.Context, email, passwordHash, fullName string, role domain.Role) error {
	now := time.Now().UTC()
	_, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"email": email}, bson.M{
		"$set":         bson.M{"password_hash": passwordHash, "full_name": fullName, "role": role, "verified": true},
		"$setOnInsert": bson.M{"_id": primitive.NewObjectID(), "created_at": now},
	}, options.Update().SetUpsert(true))
	return err
}
func (s *Mongo) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	var u domain.User
	err := s.db.Collection("users").FindOne(ctx, bson.M{"email": email}).Decode(&u)
	return u, err
}
func (s *Mongo) UserByID(ctx context.Context, id primitive.ObjectID) (domain.User, error) {
	var u domain.User
	err := s.db.Collection("users").FindOne(ctx, bson.M{"_id": id}).Decode(&u)
	return u, err
}

func (s *Mongo) ScheduleAccountDeletion(ctx context.Context, id primitive.ObjectID, requestedAt, scheduledFor time.Time) (domain.User, error) {
	var user domain.User
	err := s.db.Collection("users").FindOneAndUpdate(ctx,
		bson.M{"_id": id, "role": bson.M{"$ne": domain.RoleAdmin}},
		bson.M{"$set": bson.M{"deletion_requested_at": requestedAt, "deletion_scheduled_for": scheduledFor}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0}),
	).Decode(&user)
	return user, err
}

func (s *Mongo) CancelAccountDeletion(ctx context.Context, id primitive.ObjectID) (domain.User, error) {
	var user domain.User
	err := s.db.Collection("users").FindOneAndUpdate(ctx,
		bson.M{"_id": id, "role": bson.M{"$ne": domain.RoleAdmin}},
		bson.M{"$unset": bson.M{"deletion_requested_at": "", "deletion_scheduled_for": ""}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0}),
	).Decode(&user)
	return user, err
}
func (s *Mongo) FirstUserByRole(ctx context.Context, role domain.Role) (domain.User, error) {
	filter := bson.M{"role": role, "$or": []bson.M{{"deletion_scheduled_for": bson.M{"$exists": false}}, {"deletion_scheduled_for": bson.M{"$gt": time.Now().UTC()}}}}
	if role == domain.RolePatient {
		filter["is_developer"] = bson.M{"$ne": true}
	}
	var user domain.User
	err := s.db.Collection("users").FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "created_at", Value: 1}})).Decode(&user)
	return user, err
}
func (s *Mongo) UpdatePatientProfile(ctx context.Context, id primitive.ObjectID, profile domain.PatientProfile) (domain.User, error) {
	profile.UpdatedAt = time.Now().UTC()
	r := s.db.Collection("users").FindOneAndUpdate(
		ctx,
		bson.M{"_id": id, "role": domain.RolePatient},
		bson.M{"$set": bson.M{"patient_profile": profile}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0}),
	)
	var u domain.User
	err := r.Decode(&u)
	return u, err
}
func (s *Mongo) UpdateDeveloperTTL(ctx context.Context, id primitive.ObjectID, hours int) (domain.User, error) {
	var user domain.User
	err := s.db.Collection("users").FindOneAndUpdate(ctx,
		bson.M{"_id": id, "role": domain.RolePatient, "is_developer": true},
		bson.M{"$set": bson.M{"dev_data_ttl_hours": hours}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0}),
	).Decode(&user)
	return user, err
}
func (s *Mongo) UpdateContactProfile(ctx context.Context, id primitive.ObjectID, fullName, contactEmail, phone, city string) (domain.User, error) {
	r := s.db.Collection("users").FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"full_name": fullName, "contact_email": contactEmail, "phone": phone, "city": city}, "$unset": bson.M{"residential_address": ""}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0}),
	)
	var u domain.User
	err := r.Decode(&u)
	return u, err
}

func (s *Mongo) UpdateDoctorProfile(ctx context.Context, id primitive.ObjectID, fullName, specialization, city string, profile domain.DoctorProfile) (domain.User, error) {
	profile.About = strings.TrimSpace(profile.About)
	profile.Workplace = strings.TrimSpace(profile.Workplace)
	var user domain.User
	err := s.db.Collection("users").FindOneAndUpdate(ctx,
		bson.M{"_id": id, "role": domain.RoleDoctor},
		bson.M{"$set": bson.M{"full_name": fullName, "specialization": specialization, "city": city, "doctor_profile": profile}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0}),
	).Decode(&user)
	return user, err
}

func (s *Mongo) UpdateOnlineClinic(ctx context.Context, id primitive.ObjectID, enabled bool) (domain.User, error) {
	var user domain.User
	err := s.db.Collection("users").FindOneAndUpdate(ctx,
		bson.M{"_id": id, "role": domain.RolePatient}, bson.M{"$set": bson.M{"online_clinic": enabled}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0}),
	).Decode(&user)
	return user, err
}
func (s *Mongo) UpdateAvatar(ctx context.Context, id primitive.ObjectID, path, preset string) (domain.User, error) {
	now := time.Now().UTC()
	set := bson.M{"avatar_updated_at": now}
	unset := bson.M{}
	if path != "" {
		set["avatar_path"] = path
		unset["avatar_preset"] = ""
	} else {
		set["avatar_preset"] = preset
		unset["avatar_path"] = ""
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	var user domain.User
	err := s.db.Collection("users").FindOneAndUpdate(ctx, bson.M{"_id": id}, update, options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"password_hash": 0})).Decode(&user)
	return user, err
}
func (s *Mongo) Doctors(ctx context.Context, specialty, city string) ([]domain.User, error) {
	f := bson.M{"role": domain.RoleDoctor, "$or": []bson.M{{"deletion_scheduled_for": bson.M{"$exists": false}}, {"deletion_scheduled_for": bson.M{"$gt": time.Now().UTC()}}}}
	if specialty != "" {
		f["specialization"] = bson.M{"$regex": specialty, "$options": "i"}
	}
	if strings.TrimSpace(city) != "" {
		f["city"] = bson.M{"$regex": "^" + regexp.QuoteMeta(strings.TrimSpace(city)) + "$", "$options": "i"}
	}
	cur, err := s.db.Collection("users").Find(ctx, f, options.Find().SetProjection(bson.M{"password_hash": 0}).SetSort(bson.D{{Key: "verified", Value: -1}, {Key: "full_name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.User
	err = cur.All(ctx, &out)
	return out, err
}
func (s *Mongo) CreateAnalysis(ctx context.Context, a *domain.Analysis) error {
	if a.ID.IsZero() {
		a.ID = primitive.NewObjectID()
	}
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	_, err := s.db.Collection("analyses").InsertOne(ctx, a)
	return err
}

func (s *Mongo) RecoverExpiredOCRJobs(ctx context.Context, maxAttempts int) error {
	now := time.Now().UTC()
	collection := s.db.Collection("analyses")
	expired := bson.M{"status": domain.AnalysisStatusProcessing, "processing_lease_until": bson.M{"$lte": now}}
	if _, err := collection.UpdateMany(ctx, bson.M{"$and": []bson.M{expired, {"processing_attempt": bson.M{"$gte": maxAttempts}}}}, bson.M{
		"$set":   bson.M{"status": domain.AnalysisStatusFailed, "processing_stage": domain.ProcessingStageFailed, "processing_progress": 100, "processing_error": "Не удалось распознать документ после нескольких попыток.", "updated_at": now},
		"$unset": bson.M{"processing_worker": "", "processing_lease_until": "", "processing_next_attempt_at": ""},
	}); err != nil {
		return err
	}
	_, err := collection.UpdateMany(ctx, bson.M{"$and": []bson.M{expired, {"$or": []bson.M{{"processing_attempt": bson.M{"$lt": maxAttempts}}, {"processing_attempt": bson.M{"$exists": false}}}}}}, bson.M{
		"$set":   bson.M{"status": domain.AnalysisStatusQueued, "processing_stage": domain.ProcessingStageRetryWait, "processing_progress": 5, "processing_next_attempt_at": now, "processing_error": "Обработка была прервана и будет продолжена.", "updated_at": now},
		"$unset": bson.M{"processing_worker": "", "processing_lease_until": ""},
	})
	return err
}

func (s *Mongo) ClaimOCRJob(ctx context.Context, worker string, lease time.Duration, maxAttempts int) (domain.Analysis, error) {
	now := time.Now().UTC()
	filter := bson.M{
		"status": domain.AnalysisStatusQueued,
		"$and": []bson.M{
			{"$or": []bson.M{{"processing_next_attempt_at": bson.M{"$exists": false}}, {"processing_next_attempt_at": bson.M{"$lte": now}}}},
			{"$or": []bson.M{{"processing_attempt": bson.M{"$exists": false}}, {"processing_attempt": bson.M{"$lt": maxAttempts}}}},
		},
	}
	update := bson.M{
		"$set":   bson.M{"status": domain.AnalysisStatusProcessing, "processing_stage": domain.ProcessingStagePreprocessing, "processing_progress": 10, "processing_worker": worker, "processing_started_at": now, "processing_lease_until": now.Add(lease), "processing_error": "", "updated_at": now},
		"$inc":   bson.M{"processing_attempt": 1},
		"$unset": bson.M{"processing_next_attempt_at": "", "processing_completed_at": ""},
	}
	var job domain.Analysis
	err := s.db.Collection("analyses").FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetSort(bson.D{{Key: "processing_queued_at", Value: 1}, {Key: "created_at", Value: 1}}).SetReturnDocument(options.After)).Decode(&job)
	return job, err
}

func (s *Mongo) UpdateOCRJobProgress(ctx context.Context, id primitive.ObjectID, worker, stage string, progress int, lease time.Duration) error {
	now := time.Now().UTC()
	r, err := s.db.Collection("analyses").UpdateOne(ctx,
		bson.M{"_id": id, "status": domain.AnalysisStatusProcessing, "processing_worker": worker},
		bson.M{"$set": bson.M{"processing_stage": stage, "processing_progress": progress, "processing_lease_until": now.Add(lease), "updated_at": now}},
	)
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) CompleteOCRJob(ctx context.Context, job domain.Analysis, worker, text string, markers []domain.Marker, status, title, category string, collectedAt *time.Time) error {
	now := time.Now().UTC()
	set := bson.M{"ocr_text": text, "markers": markers, "ai_review": domain.AIReview{}, "status": status, "title": title, "category": category, "processing_stage": domain.ProcessingStageVerification, "processing_progress": 100, "processing_error": "", "processing_completed_at": now, "updated_at": now}
	if collectedAt != nil {
		set["collected_at"] = collectedAt
	}
	r, err := s.db.Collection("analyses").UpdateOne(ctx,
		bson.M{"_id": job.ID, "status": domain.AnalysisStatusProcessing, "processing_worker": worker},
		bson.M{"$set": set, "$unset": bson.M{"processing_worker": "", "processing_lease_until": "", "processing_next_attempt_at": ""}},
	)
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) RetryOCRJob(ctx context.Context, id primitive.ObjectID, worker string, next time.Time, message string, final bool) error {
	now := time.Now().UTC()
	set := bson.M{"updated_at": now, "processing_error": message}
	unset := bson.M{"processing_worker": "", "processing_lease_until": ""}
	if final {
		set["status"] = domain.AnalysisStatusFailed
		set["processing_stage"] = domain.ProcessingStageFailed
		set["processing_progress"] = 100
		set["processing_completed_at"] = now
		unset["processing_next_attempt_at"] = ""
	} else {
		set["status"] = domain.AnalysisStatusQueued
		set["processing_stage"] = domain.ProcessingStageRetryWait
		set["processing_progress"] = 5
		set["processing_next_attempt_at"] = next
	}
	r, err := s.db.Collection("analyses").UpdateOne(ctx, bson.M{"_id": id, "processing_worker": worker}, bson.M{"$set": set, "$unset": unset})
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) ReleaseOCRJob(ctx context.Context, id primitive.ObjectID, worker string) error {
	now := time.Now().UTC()
	_, err := s.db.Collection("analyses").UpdateOne(ctx, bson.M{"_id": id, "processing_worker": worker}, bson.M{
		"$set":   bson.M{"status": domain.AnalysisStatusQueued, "processing_stage": domain.ProcessingStageQueued, "processing_progress": 5, "processing_next_attempt_at": now, "updated_at": now},
		"$inc":   bson.M{"processing_attempt": -1},
		"$unset": bson.M{"processing_worker": "", "processing_lease_until": ""},
	})
	return err
}

func (s *Mongo) QueueAnalysis(ctx context.Context, id, owner primitive.ObjectID) (domain.Analysis, error) {
	now := time.Now().UTC()
	var item domain.Analysis
	err := s.db.Collection("analyses").FindOneAndUpdate(ctx, bson.M{"_id": id, "owner_id": owner}, bson.M{
		"$set":   bson.M{"status": domain.AnalysisStatusQueued, "processing_stage": domain.ProcessingStageQueued, "processing_progress": 5, "processing_attempt": 0, "processing_error": "", "processing_queued_at": now, "processing_next_attempt_at": now, "ocr_text": "", "markers": []domain.Marker{}, "ai_review": domain.AIReview{}, "updated_at": now},
		"$unset": bson.M{"processing_worker": "", "processing_started_at": "", "processing_completed_at": "", "processing_lease_until": ""},
	}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&item)
	return item, err
}

func (s *Mongo) RecordUsageEvent(ctx context.Context, kind string, user domain.User) error {
	if user.Role != domain.RolePatient || user.IsDeveloper {
		return nil
	}
	_, err := s.db.Collection("usage_events").InsertOne(ctx, bson.M{"kind": kind, "user_id": user.ID, "created_at": time.Now().UTC()})
	return err
}

func (s *Mongo) AppStats(ctx context.Context, since time.Time) (domain.AppStats, error) {
	regularUser := bson.M{"$ne": true}
	developerValues, err := s.db.Collection("users").Distinct(ctx, "_id", bson.M{"role": domain.RolePatient, "is_developer": true})
	if err != nil {
		return domain.AppStats{}, err
	}
	developerIDs := make([]primitive.ObjectID, 0, len(developerValues))
	for _, value := range developerValues {
		if id, ok := value.(primitive.ObjectID); ok {
			developerIDs = append(developerIDs, id)
		}
	}
	regularPatient := bson.M{"$nin": developerIDs}
	totalUsers, err := s.db.Collection("users").CountDocuments(ctx, bson.M{"role": domain.RolePatient, "is_developer": regularUser})
	if err != nil {
		return domain.AppStats{}, err
	}
	users, err := s.db.Collection("users").CountDocuments(ctx, bson.M{"role": domain.RolePatient, "is_developer": regularUser, "created_at": bson.M{"$gte": since}})
	if err != nil {
		return domain.AppStats{}, err
	}
	logins, err := s.db.Collection("usage_events").CountDocuments(ctx, bson.M{"kind": "login", "created_at": bson.M{"$gte": since}})
	if err != nil {
		return domain.AppStats{}, err
	}
	uploaded, err := s.db.Collection("analyses").CountDocuments(ctx, bson.M{"is_developer": regularUser, "created_at": bson.M{"$gte": since}})
	if err != nil {
		return domain.AppStats{}, err
	}
	totalUploaded, err := s.db.Collection("analyses").CountDocuments(ctx, bson.M{"is_developer": regularUser})
	if err != nil {
		return domain.AppStats{}, err
	}
	totalDoctors, err := s.db.Collection("users").CountDocuments(ctx, bson.M{"role": domain.RoleDoctor})
	if err != nil {
		return domain.AppStats{}, err
	}
	consultations, err := s.db.Collection("consultations").CountDocuments(ctx, bson.M{"source": "doctor", "patient_id": regularPatient, "created_at": bson.M{"$gte": since}})
	if err != nil {
		return domain.AppStats{}, err
	}
	appointments, err := s.db.Collection("consultations").CountDocuments(ctx, bson.M{"service_type": "appointment", "patient_id": regularPatient, "created_at": bson.M{"$gte": since}})
	if err != nil {
		return domain.AppStats{}, err
	}
	support, err := s.db.Collection("support_messages").CountDocuments(ctx, bson.M{"user_id": regularPatient, "created_at": bson.M{"$gte": since}})
	if err != nil {
		return domain.AppStats{}, err
	}
	aiRequests, err := s.db.Collection("consultations").CountDocuments(ctx, bson.M{"source": "ai", "patient_id": regularPatient, "created_at": bson.M{"$gte": since}})
	if err != nil {
		return domain.AppStats{}, err
	}
	return domain.AppStats{TotalUsers: totalUsers, NewUsers: users, Logins: logins, UploadedTests: uploaded, TotalUploadedTests: totalUploaded, TotalDoctors: totalDoctors, Consultations24h: consultations, Appointments24h: appointments, SupportMessages24h: support, AIRequests24h: aiRequests}, nil
}

// CleanupDeveloperData removes expired content from developer patient accounts.
// The account and profile remain; booked doctor slots return to availability.
func (s *Mongo) CleanupDeveloperData(ctx context.Context) ([]string, error) {
	cur, err := s.db.Collection("users").Find(ctx, bson.M{"role": domain.RolePatient, "is_developer": true})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var users []domain.User
	if err = cur.All(ctx, &users); err != nil {
		return nil, err
	}
	var paths []string
	for _, user := range users {
		hours := user.DevDataTTLHours
		if hours < 1 {
			hours = 24
		}
		cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
		analysisCur, findErr := s.db.Collection("analyses").Find(ctx, bson.M{"owner_id": user.ID, "created_at": bson.M{"$lt": cutoff}})
		if findErr != nil {
			return paths, findErr
		}
		var analyses []domain.Analysis
		if findErr = analysisCur.All(ctx, &analyses); findErr != nil {
			analysisCur.Close(ctx)
			return paths, findErr
		}
		analysisCur.Close(ctx)
		for _, analysis := range analyses {
			if analysis.StoragePath != "" {
				paths = append(paths, analysis.StoragePath)
			}
		}
		if _, err = s.db.Collection("analyses").DeleteMany(ctx, bson.M{"owner_id": user.ID, "created_at": bson.M{"$lt": cutoff}}); err != nil {
			return paths, err
		}
		var expired []domain.Consultation
		consultationCur, findErr := s.db.Collection("consultations").Find(ctx, bson.M{"patient_id": user.ID, "created_at": bson.M{"$lt": cutoff}})
		if findErr != nil {
			return paths, findErr
		}
		if findErr = consultationCur.All(ctx, &expired); findErr != nil {
			consultationCur.Close(ctx)
			return paths, findErr
		}
		consultationCur.Close(ctx)
		for _, item := range expired {
			_, _ = s.db.Collection("schedule_slots").UpdateMany(ctx, bson.M{"appointment_id": item.ID}, bson.M{"$set": bson.M{"status": "available", "updated_at": time.Now().UTC()}, "$unset": bson.M{"patient_id": "", "appointment_id": ""}})
		}
		if _, err = s.db.Collection("consultations").DeleteMany(ctx, bson.M{"patient_id": user.ID, "created_at": bson.M{"$lt": cutoff}}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("support_messages").DeleteMany(ctx, bson.M{"user_id": user.ID, "created_at": bson.M{"$lt": cutoff}}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("patient_notes").DeleteMany(ctx, bson.M{"patient_id": user.ID, "created_at": bson.M{"$lt": cutoff}}); err != nil {
			return paths, err
		}
	}
	return paths, nil
}

// CleanupScheduledAccountDeletions permanently removes expired profiles and
// every record owned by, addressed to, or created by those profiles. Files are
// returned to the caller so the process layer can remove them from disk.
func (s *Mongo) CleanupScheduledAccountDeletions(ctx context.Context, uploadDir string) ([]string, error) {
	now := time.Now().UTC()
	cur, err := s.db.Collection("users").Find(ctx, bson.M{
		"role":                   bson.M{"$ne": domain.RoleAdmin},
		"deletion_scheduled_for": bson.M{"$lte": now},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var users []domain.User
	if err = cur.All(ctx, &users); err != nil {
		return nil, err
	}
	var paths []string
	for _, user := range users {
		analysisCur, findErr := s.db.Collection("analyses").Find(ctx, bson.M{"owner_id": user.ID})
		if findErr != nil {
			return paths, findErr
		}
		var analyses []domain.Analysis
		if findErr = analysisCur.All(ctx, &analyses); findErr != nil {
			analysisCur.Close(ctx)
			return paths, findErr
		}
		analysisCur.Close(ctx)
		for _, analysis := range analyses {
			if analysis.StoragePath != "" {
				paths = append(paths, analysis.StoragePath)
			}
		}

		articleCur, findErr := s.db.Collection("clinical_articles").Find(ctx, bson.M{"doctor_id": user.ID})
		if findErr != nil {
			return paths, findErr
		}
		var articles []domain.ClinicalArticle
		if findErr = articleCur.All(ctx, &articles); findErr != nil {
			articleCur.Close(ctx)
			return paths, findErr
		}
		articleCur.Close(ctx)
		for _, article := range articles {
			if path := uploadedArticleMediaPath(uploadDir, article.CoverURL); path != "" {
				paths = append(paths, path)
			}
			for _, block := range article.Blocks {
				if path := uploadedArticleMediaPath(uploadDir, block.ImageURL); path != "" {
					paths = append(paths, path)
				}
			}
		}
		if user.AvatarPath != "" {
			paths = append(paths, user.AvatarPath)
		}

		// A patient's booked slots become available again. A departing doctor's
		// complete calendar is removed with the professional profile.
		if _, err = s.db.Collection("schedule_slots").UpdateMany(ctx, bson.M{"patient_id": user.ID}, bson.M{
			"$set":   bson.M{"status": "available", "updated_at": now},
			"$unset": bson.M{"patient_id": "", "appointment_id": ""},
		}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("schedule_slots").DeleteMany(ctx, bson.M{"doctor_id": user.ID}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("consultations").DeleteMany(ctx, bson.M{"$or": []bson.M{{"patient_id": user.ID}, {"doctor_id": user.ID}}}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("support_messages").DeleteMany(ctx, bson.M{"user_id": user.ID}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("patient_notes").DeleteMany(ctx, bson.M{"$or": []bson.M{{"patient_id": user.ID}, {"doctor_id": user.ID}}}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("ai_chats").DeleteMany(ctx, bson.M{"doctor_id": user.ID}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("clinical_articles").DeleteMany(ctx, bson.M{"doctor_id": user.ID}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("usage_events").DeleteMany(ctx, bson.M{"user_id": user.ID}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("analyses").DeleteMany(ctx, bson.M{"owner_id": user.ID}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("analyses").UpdateMany(ctx, bson.M{"shared_with": user.ID}, bson.M{"$pull": bson.M{"shared_with": user.ID}}); err != nil {
			return paths, err
		}
		if _, err = s.db.Collection("users").DeleteOne(ctx, bson.M{"_id": user.ID, "deletion_scheduled_for": bson.M{"$lte": now}}); err != nil {
			return paths, err
		}
	}
	return paths, nil
}

func uploadedArticleMediaPath(uploadDir, rawURL string) string {
	const prefix = "/api/v1/articles/media/"
	if !strings.HasPrefix(rawURL, prefix) {
		return ""
	}
	name := filepath.Base(strings.TrimPrefix(rawURL, prefix))
	if name == "." || name == "" {
		return ""
	}
	return filepath.Join(uploadDir, "article-media", name)
}
func (s *Mongo) Analysis(ctx context.Context, id primitive.ObjectID) (domain.Analysis, error) {
	var a domain.Analysis
	err := s.db.Collection("analyses").FindOne(ctx, bson.M{"_id": id}).Decode(&a)
	return a, err
}
func (s *Mongo) AnalysesFor(ctx context.Context, user primitive.ObjectID, role domain.Role) ([]domain.Analysis, error) {
	f := bson.M{"owner_id": user}
	if role == domain.RoleDoctor {
		f = bson.M{"shared_with": user}
	}
	cur, err := s.db.Collection("analyses").Find(ctx, f, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.Analysis
	err = cur.All(ctx, &out)
	return out, err
}
func (s *Mongo) Share(ctx context.Context, analysis, owner, doctor primitive.ObjectID) error {
	r, err := s.db.Collection("analyses").UpdateOne(ctx, bson.M{"_id": analysis, "owner_id": owner}, bson.M{"$addToSet": bson.M{"shared_with": doctor}, "$set": bson.M{"updated_at": time.Now().UTC()}})
	if err == nil && r.MatchedCount == 0 {
		return errors.New("analysis not found")
	}
	return err
}
func (s *Mongo) ShareAllAnalyses(ctx context.Context, owner, doctor primitive.ObjectID) error {
	_, err := s.db.Collection("analyses").UpdateMany(ctx, bson.M{"owner_id": owner}, bson.M{"$addToSet": bson.M{"shared_with": doctor}, "$set": bson.M{"updated_at": time.Now().UTC()}})
	return err
}
func (s *Mongo) UpdateAnalysisRecognition(ctx context.Context, id, owner primitive.ObjectID, text string, markers []domain.Marker, review domain.AIReview, status string, collectedAt *time.Time) error {
	set := bson.M{"ocr_text": text, "markers": markers, "ai_review": review, "status": status, "updated_at": time.Now().UTC()}
	if collectedAt != nil {
		set["collected_at"] = collectedAt
	}
	r, err := s.db.Collection("analyses").UpdateOne(ctx,
		bson.M{"_id": id, "owner_id": owner},
		bson.M{"$set": set},
	)
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) ConfirmAnalysis(ctx context.Context, id, owner primitive.ObjectID, markers []domain.Marker, review domain.AIReview) error {
	now := time.Now().UTC()
	r, err := s.db.Collection("analyses").UpdateOne(ctx,
		bson.M{"_id": id, "owner_id": owner},
		bson.M{"$set": bson.M{"markers": markers, "ai_review": review, "status": domain.AnalysisStatusReady, "processing_stage": domain.ProcessingStageCompleted, "processing_progress": 100, "processing_completed_at": now, "updated_at": now}},
	)
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}
func (s *Mongo) DeleteAnalysis(ctx context.Context, id, owner primitive.ObjectID) error {
	r, err := s.db.Collection("analyses").DeleteOne(ctx, bson.M{"_id": id, "owner_id": owner})
	if err != nil {
		return err
	}
	if r.DeletedCount == 0 {
		return mongo.ErrNoDocuments
	}
	// Consultations cannot remain useful after their source analysis is gone.
	// Cleanup is best-effort because the primary delete has already succeeded.
	_, _ = s.db.Collection("consultations").DeleteMany(ctx, bson.M{"analysis_id": id})
	return nil
}
func (s *Mongo) CreateConsultation(ctx context.Context, c *domain.Consultation) error {
	if c.ID.IsZero() {
		c.ID = primitive.NewObjectID()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = "requested"
	}
	_, err := s.db.Collection("consultations").InsertOne(ctx, c)
	return err
}
func (s *Mongo) PatientsForDoctor(ctx context.Context, doctor primitive.ObjectID) ([]domain.User, error) {
	ids, err := s.db.Collection("consultations").Distinct(ctx, "patient_id", bson.M{"doctor_id": doctor})
	if err != nil || len(ids) == 0 {
		return []domain.User{}, err
	}
	objectIDs := make([]primitive.ObjectID, 0, len(ids))
	for _, raw := range ids {
		if id, ok := raw.(primitive.ObjectID); ok {
			objectIDs = append(objectIDs, id)
		}
	}
	cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": bson.M{"$in": objectIDs}, "$or": []bson.M{{"deletion_scheduled_for": bson.M{"$exists": false}}, {"deletion_scheduled_for": bson.M{"$gt": time.Now().UTC()}}}}, options.Find().SetProjection(bson.M{"password_hash": 0}).SetSort(bson.D{{Key: "full_name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.User
	if err = cur.All(ctx, &out); err != nil {
		return nil, err
	}
	consentedRaw, err := s.db.Collection("consultations").Distinct(ctx, "patient_id", bson.M{"doctor_id": doctor, "personal_data_consent": true})
	if err != nil {
		return nil, err
	}
	consented := make(map[primitive.ObjectID]bool, len(consentedRaw))
	for _, raw := range consentedRaw {
		if id, ok := raw.(primitive.ObjectID); ok {
			consented[id] = true
		}
	}
	for i := range out {
		if consented[out[i].ID] {
			continue
		}
		out[i].FullName = "Пациент без доступа к личным данным"
		out[i].Email = ""
		out[i].PatientProfile = nil
		out[i].AvatarPath = ""
		out[i].AvatarPreset = ""
		out[i].AvatarUpdatedAt = nil
	}
	return out, nil
}
func (s *Mongo) SharedAnalysesForPatient(ctx context.Context, doctor, patient primitive.ObjectID) ([]domain.Analysis, error) {
	cur, err := s.db.Collection("analyses").Find(ctx, bson.M{"owner_id": patient, "shared_with": doctor}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.Analysis
	if err = cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Mongo) Consultations(ctx context.Context, user primitive.ObjectID, role domain.Role) ([]domain.Consultation, error) {
	key := "patient_id"
	if role == domain.RoleDoctor {
		key = "doctor_id"
	}
	cur, err := s.db.Collection("consultations").Find(ctx, bson.M{key: user}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.Consultation
	err = cur.All(ctx, &out)
	return out, err
}

func (s *Mongo) SupportMessages(ctx context.Context, user primitive.ObjectID) ([]domain.SupportMessage, error) {
	cur, err := s.db.Collection("support_messages").Find(ctx, bson.M{"user_id": user}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}).SetLimit(300))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.SupportMessage
	err = cur.All(ctx, &out)
	return out, err
}

func (s *Mongo) AllSupportMessages(ctx context.Context) ([]domain.SupportMessage, error) {
	cur, err := s.db.Collection("support_messages").Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}).SetLimit(1000))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.SupportMessage
	err = cur.All(ctx, &out)
	return out, err
}

func (s *Mongo) CreateSupportMessage(ctx context.Context, message *domain.SupportMessage) error {
	if message.ID.IsZero() {
		message.ID = primitive.NewObjectID()
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Collection("support_messages").InsertOne(ctx, message)
	return err
}
func (s *Mongo) Reply(ctx context.Context, id, doctor primitive.ObjectID, reply, status string) error {
	now := time.Now().UTC()
	r, err := s.db.Collection("consultations").UpdateOne(ctx, bson.M{"_id": id, "doctor_id": doctor}, bson.M{"$set": bson.M{"reply": reply, "status": status, "updated_at": now}, "$push": bson.M{"messages": domain.ConsultationMessage{Sender: "doctor", Text: reply, CreatedAt: now}}})
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) AppendConsultationMessage(ctx context.Context, id, user primitive.ObjectID, role domain.Role, message domain.ConsultationMessage) error {
	filter := bson.M{"_id": id}
	if role == domain.RoleDoctor {
		filter["doctor_id"] = user
	} else {
		filter["patient_id"] = user
	}
	r, err := s.db.Collection("consultations").UpdateOne(ctx, filter, bson.M{"$push": bson.M{"messages": message}, "$set": bson.M{"updated_at": message.CreatedAt}})
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) Schedule(ctx context.Context, doctor primitive.ObjectID, from, to time.Time) ([]domain.ScheduleSlot, error) {
	cur, err := s.db.Collection("schedule_slots").Find(ctx, bson.M{"doctor_id": doctor, "start_at": bson.M{"$gte": from, "$lt": to}}, options.Find().SetSort(bson.D{{Key: "start_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.ScheduleSlot
	if err = cur.All(ctx, &out); err != nil {
		return nil, err
	}
	for i := range out {
		if !out[i].PatientID.IsZero() && !out[i].AppointmentID.IsZero() {
			var consultation domain.Consultation
			if e := s.db.Collection("consultations").FindOne(ctx, bson.M{"_id": out[i].AppointmentID, "personal_data_consent": true}).Decode(&consultation); e != nil {
				continue
			}
			if patient, e := s.UserByID(ctx, out[i].PatientID); e == nil {
				out[i].PatientName = patient.FullName
			}
		}
	}
	return out, nil
}

func (s *Mongo) ReplaceSchedule(ctx context.Context, doctor primitive.ObjectID, from, to time.Time, starts []time.Time, slotMinutes int) error {
	collection := s.db.Collection("schedule_slots")
	_, err := collection.DeleteMany(ctx, bson.M{"doctor_id": doctor, "start_at": bson.M{"$gte": from, "$lt": to}, "status": "available"})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if slotMinutes != 15 && slotMinutes != 20 && slotMinutes != 30 && slotMinutes != 60 {
		slotMinutes = 30
	}
	for _, start := range starts {
		_, err = collection.UpdateOne(ctx, bson.M{"doctor_id": doctor, "start_at": start, "status": bson.M{"$ne": "booked"}}, bson.M{"$set": bson.M{"end_at": start.Add(time.Duration(slotMinutes) * time.Minute), "status": "available", "updated_at": now}, "$setOnInsert": bson.M{"_id": primitive.NewObjectID(), "doctor_id": doctor}}, options.Update().SetUpsert(true))
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Mongo) ReserveSlot(ctx context.Context, doctor, patient, appointment primitive.ObjectID, start time.Time) error {
	now := time.Now().UTC()
	r := s.db.Collection("schedule_slots").FindOneAndUpdate(ctx, bson.M{"doctor_id": doctor, "start_at": start, "status": "available"}, bson.M{"$set": bson.M{"status": "booked", "patient_id": patient, "appointment_id": appointment, "updated_at": now}})
	return r.Err()
}

func (s *Mongo) ReleaseSlot(ctx context.Context, appointment primitive.ObjectID) {
	_, _ = s.db.Collection("schedule_slots").UpdateOne(ctx, bson.M{"appointment_id": appointment}, bson.M{"$set": bson.M{"status": "available", "updated_at": time.Now().UTC()}, "$unset": bson.M{"patient_id": "", "appointment_id": ""}})
}

func (s *Mongo) PatientAccessible(ctx context.Context, doctor, patient primitive.ObjectID) bool {
	err := s.db.Collection("consultations").FindOne(ctx, bson.M{"doctor_id": doctor, "patient_id": patient}).Err()
	return err == nil
}

func (s *Mongo) PatientNotes(ctx context.Context, doctor, patient primitive.ObjectID) ([]domain.PatientNote, error) {
	cur, err := s.db.Collection("patient_notes").Find(ctx, bson.M{"doctor_id": doctor, "patient_id": patient}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.PatientNote
	err = cur.All(ctx, &out)
	return out, err
}
func (s *Mongo) CreatePatientNote(ctx context.Context, note *domain.PatientNote) error {
	now := time.Now().UTC()
	note.ID = primitive.NewObjectID()
	note.CreatedAt = now
	note.UpdatedAt = now
	_, err := s.db.Collection("patient_notes").InsertOne(ctx, note)
	return err
}

func (s *Mongo) AIChats(ctx context.Context, doctor primitive.ObjectID) ([]domain.AIChat, error) {
	cur, err := s.db.Collection("ai_chats").Find(ctx, bson.M{"doctor_id": doctor}, options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}).SetProjection(bson.M{"messages": bson.M{"$slice": -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []domain.AIChat
	err = cur.All(ctx, &out)
	return out, err
}
func (s *Mongo) CreateAIChat(ctx context.Context, chat *domain.AIChat) error {
	now := time.Now().UTC()
	chat.ID = primitive.NewObjectID()
	chat.CreatedAt = now
	chat.UpdatedAt = now
	chat.Messages = []domain.AIMessage{}
	_, err := s.db.Collection("ai_chats").InsertOne(ctx, chat)
	return err
}
func (s *Mongo) AIChat(ctx context.Context, id, doctor primitive.ObjectID) (domain.AIChat, error) {
	var out domain.AIChat
	err := s.db.Collection("ai_chats").FindOne(ctx, bson.M{"_id": id, "doctor_id": doctor}).Decode(&out)
	return out, err
}
func (s *Mongo) RenameAIChat(ctx context.Context, id, doctor primitive.ObjectID, title string) error {
	r, err := s.db.Collection("ai_chats").UpdateOne(ctx, bson.M{"_id": id, "doctor_id": doctor}, bson.M{"$set": bson.M{"title": title, "updated_at": time.Now().UTC()}})
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}
func (s *Mongo) AppendAIChat(ctx context.Context, id, doctor primitive.ObjectID, messages ...domain.AIMessage) error {
	r, err := s.db.Collection("ai_chats").UpdateOne(ctx, bson.M{"_id": id, "doctor_id": doctor}, bson.M{"$push": bson.M{"messages": bson.M{"$each": messages}}, "$set": bson.M{"updated_at": time.Now().UTC()}})
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}
func (s *Mongo) DeleteAIChat(ctx context.Context, id, doctor primitive.ObjectID) error {
	r, err := s.db.Collection("ai_chats").DeleteOne(ctx, bson.M{"_id": id, "doctor_id": doctor})
	if err == nil && r.DeletedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) ClinicalArticles(ctx context.Context, includeDrafts bool) ([]domain.ClinicalArticle, error) {
	filter := bson.M{}
	if !includeDrafts {
		filter["published"] = true
	}
	cur, err := s.db.Collection("clinical_articles").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := make([]domain.ClinicalArticle, 0)
	err = cur.All(ctx, &out)
	return out, err
}

func (s *Mongo) ClinicalArticle(ctx context.Context, id primitive.ObjectID, includeDrafts bool) (domain.ClinicalArticle, error) {
	filter := bson.M{"_id": id}
	if !includeDrafts {
		filter["published"] = true
	}
	var out domain.ClinicalArticle
	err := s.db.Collection("clinical_articles").FindOne(ctx, filter).Decode(&out)
	return out, err
}

func (s *Mongo) SaveClinicalArticle(ctx context.Context, article *domain.ClinicalArticle) error {
	now := time.Now().UTC()
	article.UpdatedAt = now
	if article.ID.IsZero() {
		article.ID = primitive.NewObjectID()
		article.CreatedAt = now
		_, err := s.db.Collection("clinical_articles").InsertOne(ctx, article)
		return err
	}
	r, err := s.db.Collection("clinical_articles").UpdateOne(ctx, bson.M{"_id": article.ID}, bson.M{"$set": bson.M{"doctor_id": article.DoctorID, "title": article.Title, "summary": article.Summary, "cover_url": article.CoverURL, "published": article.Published, "blocks": article.Blocks, "updated_at": now}})
	if err == nil && r.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (s *Mongo) DeleteClinicalArticle(ctx context.Context, id primitive.ObjectID) error {
	r, err := s.db.Collection("clinical_articles").DeleteOne(ctx, bson.M{"_id": id})
	if err == nil && r.DeletedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}
