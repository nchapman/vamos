package deploy

import (
	"context"
	"strings"
	"testing"
)

func TestFakeDockerDoesNotReuseContainerIDsAfterRemoval(t *testing.T) {
	f := newFakeDocker(t, &Manager{}, &journal{Status: Status{App: "app"}, HostID: strings.Repeat("a", 32)})
	image := "sha256:" + strings.Repeat("b", 64)
	f.images[image] = &inspection{ID: image}
	create := func(name string) string {
		t.Helper()
		if _, err := f.run(context.Background(), "docker", []string{"create", "--name", name, "--restart", "no", image}, nil); err != nil {
			t.Fatal(err)
		}
		return f.containers[name].ID
	}
	first, second := create("first"), create("second")
	if _, err := f.run(context.Background(), "docker", []string{"rm", "first"}, nil); err != nil {
		t.Fatal(err)
	}
	third := create("third")
	if third == first || third == second {
		t.Fatalf("fake Docker reused an identity: first=%s second=%s third=%s", first, second, third)
	}
}
