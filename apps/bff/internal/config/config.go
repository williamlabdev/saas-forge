package config

import (
	"os"
	"strings"
)

type Config struct {
	HTTPAddr     string
	DomainAPIURL string
	CORSOrigins  []string
	// GatewaySecret is forwarded as X-Gateway-Secret to the domain API so its
	// TKT-R7 gateway guard accepts this BFF. Must match the app's GATEWAY_SECRET.
	GatewaySecret string
}

func Load() Config {
	origins := strings.Split(os.Getenv("BFF_CORS_ORIGINS"), ",")
	if len(origins) == 1 && strings.TrimSpace(origins[0]) == "" {
		origins = []string{"http://localhost:3000"}
	}
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	addr := os.Getenv("BFF_HTTP_ADDR")
	if addr == "" {
		addr = ":4000"
	}
	base := os.Getenv("DOMAIN_API_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	return Config{
		HTTPAddr:      addr,
		DomainAPIURL:  strings.TrimRight(base, "/"),
		CORSOrigins:   origins,
		GatewaySecret: strings.TrimSpace(os.Getenv("GATEWAY_SECRET")),
	}
}
