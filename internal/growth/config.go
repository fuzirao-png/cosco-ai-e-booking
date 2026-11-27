package growth

import (
	"errors"
	"os"

	"github.com/joho/godotenv"
)

func LoadEnv() error {
	err := godotenv.Load()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func EnvOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
