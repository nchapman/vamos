# Scheduled jobs

Declare recurring commands under `services.<name>.jobs` in the manifest that owns the service. For example, a Rails `web` service can send a digest every Monday morning and prune expired sessions every 15 minutes:

```yaml
app: ledger
services:
  web:
    build: .
    jobs:
      weekly-digest:
        command: ["bin/rails", "runner", "WeeklyDigest.deliver!"]
        schedule: {every: week, on: monday, at: "09:00"}
      prune-sessions:
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

Use `timezone: UTC` for a fixed clock; in zones with daylight-saving changes a local time can be skipped or occur twice. `vamos validate` explains schedules offline. Once deployed, `vamos status` (or `vamos status --service web --json`) shows current job definitions, their next scheduled time with a UTC offset, their last retained result (if any), and whether dispatch is paused. The next time is a calendar prediction, not evidence of successful execution. A server without job support must be upgraded with `vamos server upgrade` before deploying a schedule.

## What happens at the scheduled minute?

- The host agent starts one command in **replica 1 of the current service**, not one per replica. It uses the running container's user, environment and mounts. Arguments are passed literally: no host shell or interpolation. Keep credentials in service secrets, not in command arguments.
- If a previous run of that job is still active, the tick is skipped. There is no implicit runtime limit: a long-running job can span several ticks. If you need a deadline, enforce it **inside the container**, for example `command: ["timeout", "10m", "bin/report"]` if the image includes `timeout`. Timing out just the host-side Docker client would not reliably stop the command. A tick can also be missed during downtime, deployment, maintenance, or when the target container is unavailable. There is **no catch-up, retry, or exactly-once delivery**. Make commands idempotent; for durable delivery use an application queue.
- Editing or removing a job takes effect when you deploy the target service. Its schedules follow its release through scoped deployments and rollbacks. Another service's deploy doesn't replace those definitions. As with interactive exec, replacing a container can interrupt a run.
- Job names are lowercase DNS labels scoped to the service. In split manifests, `billing/cleanup` and `notifications/cleanup` run independently. The app can have up to 32 jobs total. Overlap protection lasts while the foreground job unit is active. A detached child or a Docker client disconnect doesn't prove the in-container process stopped.

## Inspect results and output

```sh
vamos jobs                       # Most recent observed attempts, newest first
vamos jobs --service web         # Just this service; copy a run ID
vamos jobs show <id>             # Timing, release, outcome, exit code and output
vamos jobs show <id> --json      # Same detail with output as a JSON string
```

The inbox retains the latest 100 attempts **per service** on the server. The app-wide list shows the newest 100 across services. Each attempt has a stable ID. `succeeded` and `failed` are observed outcomes; `skipped` explains a known refusal or pause. `unknown` means completion hasn't been confirmed: the job may still be running, or its worker or launch acknowledgement may have been lost. Downtime doesn't create fictional attempts or backfill runs.

`show` includes the last 32 KiB of combined stdout and stderr on success or failure; a marker identifies omitted earlier output. Terminal controls are shown as text. Lists and `status` never include the output. The inbox requires the same **app-scoped `logs` permission** as service logs; `--service` is a filter, not a separate access grant. Output is stored in private host files. Vamos redacts known app secret values on a best-effort basis; don't print credentials or sensitive payloads. Keep longer-lived logs in your application's logging system.

Deploying, rolling back, or removing a schedule doesn't erase that service's inbox. Removing the service or destroying the app does. If no record appears, a host administrator can check `journalctl -u vamos-agent` for scheduler errors and `journalctl -u vamos-job-APP.SERVICE.JOB.service` for worker diagnostics. Application output isn't copied into the host journal.
