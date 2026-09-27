// Explicit repair of a previously reviewed CBC + urine microalbumin record.
// Run without -apply first, and back up the database before applying.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/httpapi"
	"github.com/repomz/lab_back/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func main() {
	raw := flag.String("id", "", "reviewed analysis ID")
	apply := flag.Bool("apply", false, "persist the split after database backup")
	flag.Parse()
	id, err := primitive.ObjectIDFromHex(*raw)
	if err != nil {
		log.Fatal("provide a valid -id")
	}
	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := store.Connect(ctx, cfg.MongoURI, cfg.MongoDatabase)
	if err != nil {
		log.Fatal("database connection failed")
	}
	count, err := httpapi.RepairBloodUrine(ctx, db, id, *apply)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("studies=%d applied=%v\n", count, *apply)
}
