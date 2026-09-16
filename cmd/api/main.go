package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/repomz/lab_back/internal/analyzer"
	"github.com/repomz/lab_back/internal/auth"
	"github.com/repomz/lab_back/internal/config"
	"github.com/repomz/lab_back/internal/httpapi"
	"github.com/repomz/lab_back/internal/processing"
	"github.com/repomz/lab_back/internal/store"
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
	if cfg.AdminLogin != "" && cfg.AdminPIN != "" {
		if hash, hashErr := auth.Hash(cfg.AdminPIN); hashErr != nil {
			log.Fatal(hashErr)
		} else if seedErr := s.EnsureSystemUser(dbCtx, cfg.AdminLogin, hash, "Администратор Lab", "admin"); seedErr != nil {
			log.Fatal(seedErr)
		}
	}
	cleanupData := func() {
		developerCtx, developerCancel := context.WithTimeout(context.Background(), 30*time.Second)
		paths, cleanupErr := s.CleanupDeveloperData(developerCtx)
		developerCancel()
		if cleanupErr != nil {
			log.Printf("developer data cleanup: %v", cleanupErr)
		}
		deletionCtx, deletionCancel := context.WithTimeout(context.Background(), 30*time.Second)
		deletedPaths, deletionErr := s.CleanupScheduledAccountDeletions(deletionCtx, cfg.UploadDir)
		deletionCancel()
		if deletionErr != nil {
			log.Printf("scheduled account deletion cleanup: %v", deletionErr)
		}
		paths = append(paths, deletedPaths...)
		for _, path := range paths {
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				log.Printf("developer upload cleanup %s: %v", path, removeErr)
			}
			_ = os.Remove(filepath.Dir(path))
		}
	}
	cleanupData()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cleanupData()
			case <-ctx.Done():
				return
			}
		}
	}()
	analysisService := analyzer.New(cfg)
	ocrQueue := processing.NewOCRQueue(cfg, s, analysisService)
	ocrQueue.Start(ctx)
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: httpapi.New(cfg, s, analysisService), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second}
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
	ocrQueue.Wait()
}
