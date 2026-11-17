package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestRuntimeClassification(t *testing.T) {
	const secret = "FAKE_SECRET_password123"
	for _, tt := range []struct {
		text string
		want error
	}{
		{"Error response from daemon: driver failed programming external connectivity on endpoint " + secret + ": Bind for 0.0.0.0:8080 failed: port is already allocated", ErrPortBind},
		{"docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint " + secret + " (abcdef): Bind for 127.0.0.1:30444 failed: port is already allocated\n\nRun 'docker run --help' for more information\n", ErrPortBind},
		{"docker: Error response from daemon: No such image: " + secret + ":missing\n", ErrImageAccess},
		{"Cannot connect to the Docker daemon at unix:///" + secret + ". Is the docker daemon running?", ErrDaemonUnavailable},
		{"docker: Error response from daemon: pull access denied for " + secret + ", repository does not exist or may require 'docker login': denied: requested access to the resource is denied.", ErrImageAccess},
		{"Error response from daemon: write /" + secret + ": no space left on device", ErrDiskFull},
		{secret + " address already in use", ErrRuntime},
		{"invalid environment variable: " + secret + " no space left on device", ErrRuntime},
		{"application says:\nError response from daemon: write /" + secret + ": no space left on device", ErrRuntime},
		{"Error response from daemon: invalid value '" + secret + ": unauthorized: authentication required'", ErrRuntime},
		{"OOMKilled " + secret, ErrRuntime},
		{strings.Repeat("x", 16385) + secret, ErrRuntime},
	} {
		err := ClassifyRuntimeFailure([]byte(tt.text))
		if !errors.Is(err, tt.want) {
			t.Errorf("got %v want %v", err, tt.want)
		}
		if strings.Contains(err.Error(), secret) || len(err.Error()) > 256 {
			t.Fatal("unsafe diagnostic")
		}
		if tt.want != ErrRuntime && RuntimeErrorCode(err) == "" {
			t.Fatal("missing code")
		}
	}
	if RuntimeErrorCode(errors.New(ErrDiskFull.Error())) != "" {
		t.Fatal("classified error text")
	}
}
