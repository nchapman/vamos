# Scheduled jobs

Declare a recurring command under `jobs` in the same `vamos.yaml` as its target service. For example, a Rails app that declares a `web` service can send a digest every Monday morning and prune expired sessions every 15 minutes:

```yaml
jobs:
  weekly-digest:
    service: web
    command: ["bin/rails", "runner", "WeeklyDigest.deliver!"]
    schedule: {every: week, on: monday, at: "09:00"}
  prune-sessions:
    service: web
    command: ["bin/rails", "sessions:prune"]
    schedule: {every: 15m}
```

**Schedules use the server's local time** unless you set `timezone` inside `schedule`. Choose one form:

| Schedule | When it runs |
| --- | --- |
| `{every: 15m}` | At :00, :15, :30 and :45 each hour. Minute intervals must divide 60 evenly; they don't restart on deploy. |
| `{every: hour}` | On the hour. |
| `{every: day, at: "02:30"}` | Every day at 02:30. |
| `{every: week, on: friday, at: "17:00"}` | Every Friday at 17:00. Use the full weekday name. |
| `{cron: "0 9 * * MON-FRI", timezone: America/New_York}` | At 09:00 New York time on weekdays. Cron fields are minute, hour, day of month, month, day of week. |

Use `timezone: UTC` for a fixed clock; in zones with daylight-saving changes a local time can be skipped or occur twice. `vamos validate` explains schedules offline. Once deployed, `vamos status` (or `vamos status --service web --json`) shows the active jobs and their next scheduled time with a UTC offset. A server without job support must be upgraded with `vamos server upgrade` before deploying a schedule.

## What happens at the scheduled minute?

- The host agent starts one command in **replica 1 of the current service**, not one per replica. It uses the running container's user, environment and mounts. Arguments are passed literally: no host shell or interpolation. Keep credentials in service secrets, not in command arguments.
- If a previous run of that job is still active, the tick is skipped. A tick can also be missed during downtime, deployment, maintenance, or when the target container is unavailable. There is **no catch-up, retry, or exactly-once delivery**. Make commands idempotent; for durable delivery use an application queue.
- Editing or removing a job takes effect when you deploy the target service. Its schedules follow its release through scoped deployments and rollbacks. Another service's deploy doesn't replace those definitions. As with interactive exec, replacing a container can interrupt a run.
- A job name is a lowercase DNS label, unique across the app (up to 32 jobs total). For host-level start/failure diagnostics use `journalctl -u vamos-job-APP.JOB.service`; the wrapper doesn't copy application stdout or stderr into the host journal. Have the application record the results you need.
