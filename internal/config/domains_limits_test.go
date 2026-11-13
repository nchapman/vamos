package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestDomainAliasLimits(t *testing.T) {
	for _, tc := range []struct {
		services, aliases int
		valid             bool
	}{{1, 8, true}, {1, 9, false}, {4, 8, true}, {5, 7, false}} {
		t.Run(fmt.Sprintf("%dx%d", tc.services, tc.aliases), func(t *testing.T) {
			a := &App{Name: "notes", Services: map[string]Service{}}
			for i := 0; i < tc.services; i++ {
				s := Service{Image: "alpine:3", Update: UpdateAuto, HTTP: &HTTP{Port: 80, Health: "/"}}
				for n := 0; n < tc.aliases; n++ {
					s.HTTP.Domains = append(s.HTTP.Domains, fmt.Sprintf("alias-%d-%d.example.com", i, n))
				}
				a.Services[fmt.Sprintf("service-%d", i)] = s
			}
			err := a.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("%+v err %v", tc, err)
			}
			if err != nil && !strings.Contains(err.Error(), "at most") {
				t.Fatal(err)
			}
		})
	}
}
