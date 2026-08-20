package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr, MongoURI, MongoDatabase, JWTSecret, UploadDir, PublicBaseURL string
	DeepSeekAPIKey, DeepSeekBaseURL, DeepSeekModel, OCRMode, TesseractLang string
	CORSOrigins                                                            []string
	MaxUploadMB                                                            int64
}

func Load() Config {
	return Config{
		HTTPAddr: env("HTTP_ADDR", ":8080"), MongoURI: env("MONGO_URI", "mongodb://localhost:27017"),
		MongoDatabase: env("MONGO_DATABASE", "lab"), JWTSecret: env("JWT_SECRET", "development-secret-change-me-please"),
		UploadDir: env("UPLOAD_DIR", "./data/uploads"), PublicBaseURL: env("PUBLIC_BASE_URL", "http://localhost:8080"),
		DeepSeekAPIKey: os.Getenv("DEEPSEEK_API_KEY"), DeepSeekBaseURL: env("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		DeepSeekModel: env("DEEPSEEK_MODEL", "deepseek-chat"), OCRMode: env("OCR_MODE", "local"),
		TesseractLang: env("TESSERACT_LANG", "rus+eng"), CORSOrigins: strings.Split(env("CORS_ORIGINS", "http://localhost:8081"), ","),
		MaxUploadMB: envInt("MAX_UPLOAD_MB", 20),
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
