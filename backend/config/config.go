package config

import (
	"os"
)

type Config struct {
	Port          string
	DBHost        string
	DBPort        string
	DBUser        string
	DBPassword    string
	DBName        string
	OllamaBaseURL string
	OllamaModel   string
}

func Load() *Config {
	return &Config{
		Port:          getEnv("PORT", "8080"),
		DBHost:        getEnv("DB_HOST", "localhost"),
		DBPort:        getEnv("DB_PORT", "5432"),
		DBUser:        getEnv("DB_USER", "flowedge"),
		DBPassword:    getEnv("DB_PASSWORD", "flowedge"),
		DBName:        getEnv("DB_NAME", "flowedge"),
		OllamaBaseURL: getEnv("OLLAMA_BASE_URL", "http://localhost:11434"),
		// 留空 = 启动时自动探测本地已安装的模型（部署到别的机器不会因默认模型不存在而报错）
		OllamaModel: getEnv("OLLAMA_MODEL", ""),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
