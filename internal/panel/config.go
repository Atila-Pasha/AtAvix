package panel

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

type Config struct {
	TrustProxy                                bool
	Addr, Driver, Database, CoreBinary, Admin string
	PanelURL, SubscriptionURL, ProxyURL       *url.URL
	PasswordHash                              []byte
	Reality                                   *Reality
	Telegram                                  *Telegram
}

func ReadConfig() (Config, error) {
	cfg := Config{
		TrustProxy: env("TRUST_PROXY", "") == "true",
		Addr:       env("HTTP_ADDR", "127.0.0.1:8080"), CoreBinary: env("XRAY_BIN", "xray"),
		Admin: env("ADMIN_USERNAME", "admin"), Database: env("DATABASE_URL", "atavix.sqlite"),
	}
	cfg.Driver = "sqlite3"
	if strings.HasPrefix(cfg.Database, "postgres://") || strings.HasPrefix(cfg.Database, "postgresql://") {
		cfg.Driver = "pgx"
	}
	var err error
	for _, item := range []struct {
		name, fallback string
		dst            **url.URL
	}{
		{"PANEL_URL", "http://127.0.0.1:8080", &cfg.PanelURL},
		{"SUBSCRIPTION_URL", env("PANEL_URL", "http://127.0.0.1:8080"), &cfg.SubscriptionURL},
		{"PROXY_URL", env("SUBSCRIPTION_URL", env("PANEL_URL", "http://127.0.0.1:8080")), &cfg.ProxyURL},
	} {
		*item.dst, err = origin(env(item.name, item.fallback))
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", item.name, err)
		}
	}
	cfg.Reality, err = readReality()
	if err != nil {
		return cfg, err
	}
	cfg.Telegram, err = readTelegram()
	if err != nil {
		return cfg, err
	}
	if !usernamePattern.MatchString(cfg.Admin) {
		return cfg, errors.New("ADMIN_USERNAME must contain 3–32 letters, numbers, dots, dashes or underscores")
	}
	password, err := os.ReadFile(env("ADMIN_PASSWORD_FILE", ".atavix/admin-password"))
	if err != nil {
		return cfg, fmt.Errorf("read admin password file: %w", err)
	}
	password = []byte(strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r"))
	if len(password) < 12 || len(password) > 72 {
		return cfg, errors.New("admin password must be 12–72 bytes")
	}
	cfg.PasswordHash, err = bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	return cfg, err
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func origin(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("enter an HTTP or HTTPS origin with a host and optional port")
	}
	parsed.Path = ""
	return parsed, nil
}
