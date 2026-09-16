package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr, MongoURI, MongoDatabase, JWTSecret, UploadDir, PublicBaseURL string
	DeepSeekAPIKey, DeepSeekBaseURL, DeepSeekModel, OCRMode, TesseractLang string
	AdminLogin, AdminPIN                                                   string
	CORSOrigins                                                            []string
	MaxUploadMB                                                            int64
	DeepSeekRequestsPerMinute, DeepSeekRequestsPerHour                     int
	DeepSeekMaxConcurrent, DeepSeekTimeoutSeconds                          int
	AIUserRequestsPerMinute, AIUserRequestsPerHour                         int
}

func Load() Config {
	return Config{
		HTTPAddr: env("HTTP_ADDR", ":8080"), MongoURI: env("MONGO_URI", "mongodb://localhost:27017"),
		MongoDatabase: env("MONGO_DATABASE", "lab"), JWTSecret: env("JWT_SECRET", "development-secret-change-me-please"),
		UploadDir: env("UPLOAD_DIR", "./data/uploads"), PublicBaseURL: env("PUBLIC_BASE_URL", "http://localhost:8080"),
		DeepSeekAPIKey: os.Getenv("DEEPSEEK_API_KEY"), DeepSeekBaseURL: env("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		DeepSeekModel: env("DEEPSEEK_MODEL", "deepseek-v4-flash"), OCRMode: env("OCR_MODE", "local"),
		TesseractLang: env("TESSERACT_LANG", "rus+eng"), CORSOrigins: strings.Split(env("CORS_ORIGINS", "http://localhost:8081"), ","),
		AdminLogin: strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_LOGIN"))), AdminPIN: strings.TrimSpace(os.Getenv("ADMIN_PIN")),
		MaxUploadMB:               envInt("MAX_UPLOAD_MB", 20),
		DeepSeekRequestsPerMinute: int(envInt("DEEPSEEK_REQUESTS_PER_MINUTE", 30)),
		DeepSeekRequestsPerHour:   int(envInt("DEEPSEEK_REQUESTS_PER_HOUR", 300)),
		DeepSeekMaxConcurrent:     int(envInt("DEEPSEEK_MAX_CONCURRENT", 2)),
		DeepSeekTimeoutSeconds:    int(envInt("DEEPSEEK_TIMEOUT_SECONDS", 45)),
		AIUserRequestsPerMinute:   int(envInt("AI_USER_REQUESTS_PER_MINUTE", 6)),
		AIUserRequestsPerHour:     int(envInt("AI_USER_REQUESTS_PER_HOUR", 60)),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func envInt(key string, fallback int64) int64 {
	v, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err == nil && v > 0 {
		return v
	}
	return fallback
}
