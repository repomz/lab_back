package store

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type HealthSummary struct {
	OwnerID     primitive.ObjectID `bson:"owner_id" json:"-"`
	Fingerprint string             `bson:"fingerprint" json:"-"`
	Summary     string             `bson:"summary" json:"summary"`
	GeneratedAt time.Time          `bson:"generated_at" json:"generated_at"`
}

func (s *Mongo) HealthSummary(ctx context.Context, owner primitive.ObjectID, fingerprint string) (HealthSummary, error) {
	var result HealthSummary
	err := s.db.Collection("health_summaries").FindOne(ctx, bson.M{"_id": owner.Hex() + ":" + fingerprint}).Decode(&result)
	return result, err
}

func (s *Mongo) SaveHealthSummary(ctx context.Context, summary HealthSummary) error {
	_, err := s.db.Collection("health_summaries").UpdateOne(ctx, bson.M{"_id": summary.OwnerID.Hex() + ":" + summary.Fingerprint}, bson.M{"$setOnInsert": summary}, options.Update().SetUpsert(true))
	return err
}
