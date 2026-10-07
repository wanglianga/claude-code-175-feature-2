package config

import (
	"os"
	"strconv"
)

type Config struct {
	HTTPAddr      string
	DatabaseURL   string
	RedisAddr     string
	KafkaBrokers  string
	KafkaTopic    string
	KafkaDisabled bool
	AutoMigrate   bool
	Seed          bool
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Load() Config {
	return Config{
		HTTPAddr:      getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:   getenv("DATABASE_URL", "postgres://app@127.0.0.1:5433/deposit?sslmode=disable"),
		RedisAddr:     getenv("REDIS_ADDR", "127.0.0.1:6379"),
		KafkaBrokers:  getenv("KAFKA_BROKERS", "127.0.0.1:9092"),
		KafkaTopic:    getenv("KAFKA_TOPIC", "lease.events"),
		KafkaDisabled: getenv("KAFKA_DISABLED", "false") == "true",
		AutoMigrate:   getenv("AUTO_MIGRATE", "true") == "true",
		Seed:          getenv("SEED", "true") == "true",
	}
}

// Atoi 是给脚本/测试用的简易辅助。
func Atoi(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}
