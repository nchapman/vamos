package config

import "testing"

func TestResourcesRejectQuotaBelowDockerPeriodMinimum(t *testing.T) {
	// Docker maps --cpus onto a 100ms period. Linux requires at least 1ms
	// quota, so values below 0.01 cannot be enforced through this interface.
	for _, cpu := range []float64{0.001, 0.009} {
		if err := (&Resources{CPUs: &cpu}).Validate(); err == nil {
			t.Fatalf("accepted CPU limit %g below Docker's usable minimum", cpu)
		}
	}
}
