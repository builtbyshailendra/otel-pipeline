package scrubprocessor

import (
	"fmt"
	"strings"
)

// Config for the scrub processor.
//
//	processors:
//	  scrub:
//	    drop_keys: [http.request.header.authorization, password]
//	    detectors: [card, email, bearer, jwt, aws_key]
//	    allow_keys: [order.id]
type Config struct {
	// DropKeys are attribute keys removed outright, wherever they appear
	// (matched case-insensitively). For values that must never leave.
	DropKeys []string `mapstructure:"drop_keys"`
	// Detectors are the value patterns masked inside any string value.
	Detectors []string `mapstructure:"detectors"`
	// AllowKeys are never inspected (e.g. an ID that happens to look like a
	// card number but is known not to be one).
	AllowKeys []string `mapstructure:"allow_keys"`
}

func createDefaultConfig() *Config {
	return &Config{
		DropKeys: []string{
			"http.request.header.authorization", "http.request.header.cookie",
			"http.response.header.set-cookie", "password", "user.password",
		},
		Detectors: []string{"card", "email", "bearer", "jwt", "aws_key"},
	}
}

// Validate rejects detectors this processor doesn't know, so a typo in the
// config fails at startup instead of silently scrubbing nothing.
func (c *Config) Validate() error {
	for _, d := range c.Detectors {
		if _, ok := detectors[strings.ToLower(d)]; !ok {
			return fmt.Errorf("unknown detector %q (known: card, email, bearer, jwt, aws_key)", d)
		}
	}
	return nil
}
