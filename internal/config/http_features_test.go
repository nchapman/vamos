package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestHTTPFeatureSwitches(t *testing.T) {
	for _, feature := range []string{"waf", "cache", "compression"} {
		for _, value := range []string{"true", "false", "invalid", "[]"} {
			t.Run(feature+"/"+value, func(t *testing.T) {
				app, err := config.Decode(strings.NewReader(fmt.Sprintf("app: notes\nservices:\n  web:\n    build: .\n    http:\n      port: 8080\n      %s: %s\n", feature, value)))
				if value == "invalid" || value == "[]" {
					if err == nil {
						t.Fatal("expected invalid boolean to be rejected")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				h := app.Services["web"].HTTP
				switches := map[string]*bool{"waf": h.WAF, "cache": h.Cache, "compression": h.Compression}
				enabled := map[string]bool{"waf": h.WAFEnabled(), "cache": h.CacheEnabled(), "compression": h.CompressionEnabled()}
				for name, ptr := range switches {
					want := name != feature || value == "true"
					if enabled[name] != want {
						t.Fatalf("%s enabled = %v, want %v", name, enabled[name], want)
					}
					if want && ptr != nil {
						t.Fatalf("%s: true should normalize to omission", name)
					}
					if !want && (ptr == nil || *ptr) {
						t.Fatalf("%s: false must be retained", name)
					}
				}
			})
		}
	}
}
