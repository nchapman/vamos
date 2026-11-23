package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Offline validation also works on clients without a system zone database.
	"unicode/utf8"

	"github.com/robfig/cron/v3"
)

// A Job belongs to one service and travels with its immutable release history.
type Job struct {
	Command  []string `yaml:"command"`
	Schedule Schedule `yaml:"schedule"`
}

type Schedule struct {
	Cron     string `yaml:"cron,omitempty"`
	Every    string `yaml:"every,omitempty"`
	On       string `yaml:"on,omitempty"`
	At       string `yaml:"at,omitempty"`
	Timezone string `yaml:"timezone,omitempty"`
}

var jobCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Compile accepts one minute-resolution, wall-clock schedule. The parser's
// default location is the caller's time zone; host callers pass local times.
func (s Schedule) Compile() (cron.Schedule, error) {
	expr, err := s.expression()
	if err != nil {
		return nil, err
	}
	if s.Timezone != "" {
		if s.Timezone == "Local" || len(s.Timezone) > 128 || strings.ContainsAny(s.Timezone, " \t\r\n=") {
			return nil, errors.New("timezone must be an IANA zone name")
		}
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return nil, fmt.Errorf("unknown timezone %q", s.Timezone)
		}
		expr = "CRON_TZ=" + s.Timezone + " " + expr
	}
	return jobCronParser.Parse(expr)
}

func (s Schedule) expression() (string, error) {
	if s.Cron != "" {
		if s.Every != "" || s.On != "" || s.At != "" {
			return "", errors.New("cron cannot be combined with every, on or at")
		}
		if len(s.Cron) > 256 || len(strings.Fields(s.Cron)) != 5 || strings.ContainsAny(s.Cron, "=@\r\n") {
			return "", errors.New("cron must be a five-field expression; use timezone for its zone")
		}
		return s.Cron, nil
	}
	if s.Every == "day" || s.Every == "week" {
		t, err := time.Parse("15:04", s.At)
		if err != nil || t.Format("15:04") != s.At {
			return "", errors.New("at must be a quoted 24-hour HH:MM for day or week")
		}
		day := "*"
		if s.Every == "day" && s.On != "" {
			return "", errors.New("on is only valid with every: week")
		}
		if s.Every == "week" {
			day = ""
			for d := time.Sunday; d <= time.Saturday; d++ {
				if strings.EqualFold(s.On, d.String()) {
					day = strconv.Itoa(int(d))
					break
				}
			}
			if day == "" {
				return "", errors.New("on must name a weekday for every: week")
			}
		}
		return fmt.Sprintf("%d %d * * %s", t.Minute(), t.Hour(), day), nil
	}
	if s.On != "" || s.At != "" {
		return "", errors.New("intervals cannot specify on or at")
	}
	if s.Every == "hour" {
		return "0 * * * *", nil
	}
	if n, unit, ok := strings.Cut(s.Every, "m"); ok && unit == "" {
		step, err := strconv.Atoi(n)
		if err == nil && step > 0 && step <= 60 && 60%step == 0 && strconv.Itoa(step) == n {
			return fmt.Sprintf("*/%d * * * *", step), nil
		}
	}
	return "", errors.New("specify cron or every: day, week, hour, or a minute interval dividing an hour (e.g. 15m)")
}

func (s Schedule) Description() string {
	parts := []string{s.Cron}
	if s.Every != "" {
		parts = []string{"every", s.Every}
		if s.On != "" {
			parts = append(parts, "on", s.On)
		}
		if s.At != "" {
			parts = append(parts, "at", s.At)
		}
	}
	zone := s.Timezone
	if zone == "" {
		zone = "server local time"
	}
	return strings.Join(parts, " ") + " (" + zone + ")"
}

func (j Job) validate() error {
	if len(j.Command) == 0 || len(j.Command) > 256 || j.Command[0] == "" {
		return errors.New("command requires an executable and at most 256 arguments")
	}
	total := 0
	for _, arg := range j.Command {
		total += len(arg)
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') || len(arg) > 8192 || total > 32<<10 {
			return errors.New("invalid or oversized command arguments")
		}
	}
	_, err := j.Schedule.Compile()
	return err
}
