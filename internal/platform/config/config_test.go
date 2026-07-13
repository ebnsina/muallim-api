package config

import (
	"strings"
	"testing"
)

// base is a Config that validates, so each test can break exactly one thing.
func base() Config {
	return Config{
		Env:         EnvDevelopment,
		Addr:        ":8080",
		DatabaseURL: "postgres://muallim@localhost/muallim",
		JWTSecret:   strings.Repeat("k", MinJWTSecretLength),
		WebBaseURL:  "https://app.example.com",
		APIBaseURL:  "https://api.example.com",
	}
}

func hasError(err error, substr string) bool {
	return err != nil && strings.Contains(err.Error(), substr)
}

/*
The fake gateway settles a course's access on a signed webhook, and the signature
is a shared secret. A default secret would authenticate a forged "paid" webhook
from anybody who read the source. So the config refuses to boot it unsafely.
*/
func TestTheFakeGatewayWillNotBootUnsafely(t *testing.T) {
	t.Run("enabled with no secret is refused", func(t *testing.T) {
		c := base()
		c.FakeGatewayEnabled = true
		if err := c.validate(); !hasError(err, "MUALLIM_FAKE_GATEWAY_SECRET is unset") {
			t.Fatalf("an enabled fake gateway with no secret was accepted: %v", err)
		}
	})

	t.Run("the shipped default secret is refused by name", func(t *testing.T) {
		c := base()
		c.FakeGatewayEnabled = true
		c.FakeGatewaySecret = "fake-gateway-secret"
		if err := c.validate(); !hasError(err, "shipped default") {
			t.Fatalf("the public default secret was accepted: %v", err)
		}
	})

	t.Run("refused in production even with a secret", func(t *testing.T) {
		c := base()
		c.Env = EnvProduction
		c.JWTSecret = strings.Repeat("k", MinJWTSecretLength)
		c.SMTPHost = "smtp.example.com"
		c.FakeGatewayEnabled = true
		c.FakeGatewaySecret = "a-real-and-private-secret"
		// Only the fake-gateway error is asserted; other prod requirements may also
		// fail, which is fine — the point is that this one fires.
		if err := c.validate(); !hasError(err, "must be off in production") {
			t.Fatalf("the fake gateway was allowed in production: %v", err)
		}
	})

	t.Run("enabled with a real secret outside production is fine", func(t *testing.T) {
		c := base()
		c.FakeGatewayEnabled = true
		c.FakeGatewaySecret = "a-real-and-private-secret"
		if err := c.validate(); err != nil {
			t.Fatalf("a properly configured fake gateway was refused: %v", err)
		}
	})

	t.Run("disabled needs no secret", func(t *testing.T) {
		c := base()
		c.FakeGatewayEnabled = false
		if err := c.validate(); err != nil {
			t.Fatalf("a disabled fake gateway was refused: %v", err)
		}
	})
}
