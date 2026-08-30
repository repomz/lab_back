package store

import (
	"context"
	"errors"
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
	_, err = s.db.Collection("users").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true)})
	if err != nil {
		return nil, err
	}
	_, err = s.db.Collection("schedule_slots").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "doctor_id", Value: 1}, {Key: "start_at", Value: 1}}, Options: options.Index().SetUnique(true)})
	return s, err
}
func (s *Mongo) CreateUser(ctx context.Context, u *domain.User) error {
	u.ID = primitive.NewObjectID()
	u.CreatedAt = time.Now().UTC()
	_, err := s.db.Collection("users").InsertOne(ctx, u)
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
func (s *Mongo) Doctors(ctx context.Context, specialty string) ([]domain.User, error) {
	f := bson.M{"role": domain.RoleDoctor}
	if specialty != "" {
		f["specialization"] = bson.M{"$regex": specialty, "$options": "i"}
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
	a.ID = primitive.NewObjectID()
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	_, err := s.db.Collection("analyses").InsertOne(ctx, a)
	return err
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
func (s *Mongo) UpdateAnalysisRecognition(ctx context.Context, id, owner primitive.ObjectID, text string, markers []domain.Marker, review domain.AIReview, status string) error {
	r, err := s.db.Collection("analyses").UpdateOne(ctx,
		bson.M{"_id": id, "owner_id": owner},
		bson.M{"$set": bson.M{"ocr_text": text, "markers": markers, "ai_review": review, "status": status, "updated_at": time.Now().UTC()}},
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
	cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": bson.M{"$in": objectIDs}}, options.Find().SetProjection(bson.M{"password_hash": 0}).SetSort(bson.D{{Key: "full_name", Value: 1}}))
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
func (s *Mongo) Reply(ctx context.Context, id, doctor primitive.ObjectID, reply, status string) error {
	r, err := s.db.Collection("consultations").UpdateOne(ctx, bson.M{"_id": id, "doctor_id": doctor}, bson.M{"$set": bson.M{"reply": reply, "status": status, "updated_at": time.Now().UTC()}})
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

func (s *Mongo) ReplaceSchedule(ctx context.Context, doctor primitive.ObjectID, from, to time.Time, starts []time.Time) error {
	collection := s.db.Collection("schedule_slots")
	_, err := collection.DeleteMany(ctx, bson.M{"doctor_id": doctor, "start_at": bson.M{"$gte": from, "$lt": to}, "status": "available"})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, start := range starts {
		_, err = collection.UpdateOne(ctx, bson.M{"doctor_id": doctor, "start_at": start, "status": bson.M{"$ne": "booked"}}, bson.M{"$set": bson.M{"end_at": start.Add(30 * time.Minute), "status": "available", "updated_at": now}, "$setOnInsert": bson.M{"_id": primitive.NewObjectID(), "doctor_id": doctor}}, options.Update().SetUpsert(true))
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
