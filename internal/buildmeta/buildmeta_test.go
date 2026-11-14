package buildmeta

import (
	"strings"
	"testing"
)

func TestPairing(t *testing.T) {
	stamp, err := Encode(strings.Repeat("a", 64), "0.2.0-dev")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := Encode(strings.Repeat("b", 64), "0.2.0-dev")
	for _, tt := range []struct {
		name, client, artifact string
		ok                     bool
	}{
		{"match", stamp, "binary-prefix" + stamp + "binary-suffix", true},
		{"missing client", "", stamp, false},
		{"malformed client", stamp + "extra", stamp, false},
		{"old raw executor", stamp, "binary", false},
		{"different source", stamp, other, false},
		{"duplicate", stamp, stamp + stamp, false},
		{"two identities", stamp, stamp + other, false},
		{"invalid marker", stamp, strings.Replace(stamp, strings.Repeat("a", 64), strings.Repeat("z", 64), 1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := Check(tt.client, []byte(tt.artifact)); (err == nil) != tt.ok {
				t.Fatalf("Check: %v", err)
			}
		})
	}
}

func TestVersionAndCurrent(t *testing.T) {
	original := Stamp
	t.Cleanup(func() { Stamp = original })
	Stamp = ""
	if !strings.Contains(Version(), "unversioned") {
		t.Fatal(Version())
	}
	if _, err := Current(); err == nil {
		t.Fatal("expected unversioned Current error")
	}
	Stamp, _ = Encode(strings.Repeat("c", 64), "v1.2.3")
	if got := Version(); got != "v1.2.3 (build "+strings.Repeat("c", 64)+")" {
		t.Fatal(got)
	}
	info, err := Current()
	if err != nil || info.Label != "v1.2.3" || info.ID != strings.Repeat("c", 64) {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestLabels(t *testing.T) {
	for _, label := range []string{"", "v1\n-X=other", "two words", strings.Repeat("a", 65), "-bad"} {
		if ValidLabel(label) {
			t.Fatalf("accepted %q", label)
		}
	}
	for _, label := range []string{"dev", "v1.2.3-rc.1+metadata"} {
		if !ValidLabel(label) {
			t.Fatalf("rejected %q", label)
		}
	}
}
