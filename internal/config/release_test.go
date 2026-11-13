package config

import (
	"strings"
	"testing"
)

func TestReleaseDirective(t *testing.T) {
	_, err := Decode(strings.NewReader("app: notes\nrelease:\n  service: web\n  command: [bin/release]\nservices:\n  web: {image: ruby:3.4}\n"))
	if err != nil {
		t.Fatal(err)
	}
}

func TestReleaseDirectiveRejectsInvalidInputs(t *testing.T) {
	for _, release := range []string{
		"{}", "{service: missing, command: [bin/release]}", "{service: web, command: []}",
		"{service: web, command: ['']}", "{service: web, command: bin/release}",
		"{service: web, command: [bin/release], retry: true}",
	} {
		t.Run(release, func(t *testing.T) {
			_, err := Decode(strings.NewReader("app: notes\nrelease: " + release + "\nservices:\n  web: {image: ruby:3.4}\n"))
			if err == nil {
				t.Fatal("accepted invalid release directive")
			}
		})
	}
}
