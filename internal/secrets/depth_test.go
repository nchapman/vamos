package secrets

import (
	"strings"
	"testing"
)

func TestUniqueJSONRejectsExcessiveNesting(t *testing.T) {
	data := []byte(strings.Repeat("[", 1000) + "0" + strings.Repeat("]", 1000))
	if uniqueJSON(data) {
		t.Fatal("accepted excessive nesting")
	}
	if !uniqueJSON([]byte(`{"versions":{"version":{"name":"key","value":"value"}}}`)) {
		t.Fatal("rejected ordinary store depth")
	}
}
