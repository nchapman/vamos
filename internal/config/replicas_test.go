package config

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestReplicasSchema(t *testing.T) {
	base := "app: notes\nservices:\n  web:\n    image: demo\n"
	omitted, err := Decode(strings.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, -1, 33, math.MaxInt} {
		if _, err := Decode(strings.NewReader(base + fmt.Sprintf("    replicas: %d\n", n))); err == nil {
			t.Fatalf("accepted %d", n)
		}
	}
	if _, err := Decode(strings.NewReader(base + "    replicas: 32\n")); err != nil {
		t.Fatal(err)
	}
	// The default singleton plus MaxInt must not wrap the aggregate negative.
	if _, err := Decode(strings.NewReader(base + fmt.Sprintf("  worker:\n    image: demo\n    replicas: %d\n", math.MaxInt))); err == nil {
		t.Fatal("aggregate overflow accepted")
	}
	one, err := Decode(strings.NewReader(base + "    replicas: 1\n"))
	if err != nil || !reflect.DeepEqual(one, omitted) {
		t.Fatalf("one not normalized: %v", err)
	}
	if _, err := Decode(strings.NewReader(base + "    replicas: 2\n    volume: {name: data, mount: /data}\n")); err == nil {
		t.Fatal("replicated volume accepted")
	}
	many := "app: notes\nservices:\n"
	for i := 0; i < 5; i++ {
		many += fmt.Sprintf("  svc%d:\n    image: demo\n    replicas: 8\n", i)
	}
	if _, err := Decode(strings.NewReader(many)); err == nil {
		t.Fatal("app instance bound ignored")
	}
}
