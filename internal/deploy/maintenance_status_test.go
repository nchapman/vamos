package deploy

import (
	"context"
	"strings"
	"testing"
)

func TestDestroyStatusPreservesFailureAfterCurrentContainerRemoval(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "context", false)
	old, next, err := f.m.destroyRoutes(j)
	if err != nil {
		t.Fatal(err)
	}
	j.Maintenance = &maintenance{Operation: strings.Repeat("e", 32), Action: "destroy", Plan: Plan{Action: "destroy", App: j.App}, OldCaddy: old, NewCaddy: next}
	j.Phase = "destroying"
	j.LastError = "image removal needs operator recovery"
	if err := f.m.save(j); err != nil {
		t.Fatal(err)
	}
	delete(f.containers, j.Current.Container)
	r.Action = "status"
	r.Config = nil
	r.Operation = ""
	response, err := f.m.Execute(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status.LastError != j.LastError {
		t.Fatalf("expected maintenance failure, got %q", response.Status.LastError)
	}
}
