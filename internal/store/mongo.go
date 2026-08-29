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
	c.ID = primitive.NewObjectID()
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
