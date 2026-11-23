package config

import (
	"strings"
	"testing"
	"time"
)

func TestSchedulesUseWallClockTime(t *testing.T) {
	// Sunday night in a stand-in for the server's local zone, not the client's.
	server := time.FixedZone("server", 2*60*60)
	start := time.Date(2026, 7, 5, 23, 52, 12, 0, server)
	for _, tc := range []struct {
		s    Schedule
		next string
	}{
		{Schedule{Every: "15m"}, "2026-07-06T00:00:00+02:00"},
		{Schedule{Every: "day", At: "02:30"}, "2026-07-06T02:30:00+02:00"},
		{Schedule{Every: "week", On: "monday", At: "09:00"}, "2026-07-06T09:00:00+02:00"},
		{Schedule{Cron: "0 9 * * MON-FRI", Timezone: "America/New_York"}, "2026-07-06T15:00:00+02:00"},
	} {
		s, err := tc.s.Compile()
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Next(start).Format(time.RFC3339); got != tc.next {
			t.Errorf("%v: next %s, want %s", tc.s, got, tc.next)
		}
	}
}

func TestSchedulesRejectAmbiguity(t *testing.T) {
	for _, s := range []Schedule{{}, {Cron: "0 1 * * *", Every: "hour"}, {Cron: "@daily"}, {Cron: "* * * * * *"}, {Cron: "TZ=UTC 0 * * *"}, {Every: "hour", Timezone: "Unknown/Zone"}, {Every: "day", At: "25:00"}, {Every: "day", At: "04:00", On: "monday"}, {Every: "week", At: "09:00"}, {Every: "15m", At: "03:00"}, {Every: "7m"}, {Every: "0m"}} {
		if _, err := s.Compile(); err == nil {
			t.Errorf("accepted %+v", s)
		}
	}
}

func TestJobsManifestBindsCommandToService(t *testing.T) {
	const prefix = "app: notes\nservices:\n  worker: {image: 'worker:1'}\njobs:\n  digest:\n    service: worker\n    command: [send, 'two words']\n    schedule: {every: week, on: monday, at: '09:00'}\n"
	a, err := Decode(strings.NewReader(prefix))
	if err != nil {
		t.Fatal(err)
	}
	job := a.Services["worker"].Jobs["digest"]
	if job.Command[1] != "two words" || job.Schedule.On != "monday" {
		t.Fatal(job)
	}
	for _, change := range [][2]string{{"service: worker", "service: other"}, {"[send, 'two words']", "[]"}, {"on: monday", "onn: monday"}, {"command: [send, 'two words']", "command: send"}, {"digest:", "BAD!:"}} {
		if _, err := Decode(strings.NewReader(strings.ReplaceAll(prefix, change[0], change[1]))); err == nil {
			t.Errorf("accepted %q", change)
		}
	}
}
