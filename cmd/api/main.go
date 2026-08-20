package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IdrisovMarat/lab_back/internal/analyzer"
	"github.com/IdrisovMarat/lab_back/internal/config"
	"github.com/IdrisovMarat/lab_back/internal/httpapi"
	"github.com/IdrisovMarat/lab_back/internal/store"
)

func main() {
	cfg := config.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dbCtx, dbCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dbCancel()
	s, err := store.Connect(dbCtx, cfg.MongoURI, cfg.MongoDatabase)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: httpapi.New(cfg, s, analyzer.New(cfg)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second}
	go func() {
		log.Printf("lab api listening on %s", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = server.Shutdown(shutdown)
}
