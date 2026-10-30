# Migrating from Watchtower

dockpaum reads Watchtower's labels, and on request its environment variables, so most setups only swap the container.

1. Remove Watchtower: `docker rm -f watchtower`.
2. Take dockpaum's [docker-compose.yml](../docker-compose.yml) and add your Watchtower variables plus `DOCKPAUM_WATCHTOWER_ENV`:

   ```yaml
   services:
     dockpaum:
       environment:
         DOCKPAUM_WATCHTOWER_ENV: "true"
         WATCHTOWER_POLL_INTERVAL: "86400"
         WATCHTOWER_CLEANUP: "true"
         WATCHTOWER_NOTIFICATION_URL: ntfy://ntfy.sh/my-topic
   ```

3. `docker compose up -d`. dockpaum starts in observe mode: it reports what it would update but changes nothing. Check `docker exec dockpaum /dockpaum plan`.
4. When that looks right, add `DOCKPAUM_DRY_RUN: "false"` and run `docker compose up -d` again.

Without `DOCKPAUM_WATCHTOWER_ENV=true`, dockpaum ignores `WATCHTOWER_*`, `REPO_USER` and `REPO_PASS` and logs a warning listing them. It also warns about every variable it can't honor. Watchtower's command-line flags don't work, except container names as arguments; put settings in the environment or the [config file](configuration.md).

## What each setting becomes

| Watchtower | dockpaum |
|---|---|
| `WATCHTOWER_POLL_INTERVAL`, `WATCHTOWER_SCHEDULE` | `schedule.interval`, `schedule.cron` (the six-field format works) |
| `WATCHTOWER_UPDATE_ON_START`, `WATCHTOWER_RUN_ONCE` | `schedule.run_on_start`, `dockpaum --once` |
| `WATCHTOWER_CLEANUP`, `WATCHTOWER_TIMEOUT` | `update.cleanup_images`, `update.stop_timeout` |
| `WATCHTOWER_LABEL_ENABLE`, `WATCHTOWER_SCOPE` | `filters.label_enable`, `filters.scope` |
| `WATCHTOWER_INCLUDE_STOPPED` (and `INCLUDE_RESTARTING`) | `filters.include_stopped` |
| `WATCHTOWER_DISABLE_CONTAINERS`, container names | `filters.exclude_names`, `filters.include_names` |
| `WATCHTOWER_MONITOR_ONLY` | `dry_run` |
| `WATCHTOWER_ROLLING_RESTART` | `update.strategy: rolling` (the default) |
| `WATCHTOWER_COOLDOWN_DELAY` | `min_image_age` of the default policy |
| `WATCHTOWER_LIFECYCLE_HOOKS` | `hooks.label_hooks` |
| `WATCHTOWER_HTTP_API_TOKEN`, `_UPDATE`, `_METRICS`, `_HOST`, `_PORT` | `http.token`, `http.update_api`, `http.watchtower_compat`, `http.addr`, `http.port` |
| `WATCHTOWER_NOTIFICATION_URL`, `WATCHTOWER_NOTIFICATIONS` and their variables | `notify`, with the same URLs |
| `WATCHTOWER_DEBUG`, `_TRACE`, `_LOG_LEVEL`, `_LOG_FORMAT` | `log.level`, `log.format` |
| `REPO_USER`, `REPO_PASS` | a `registry` entry for Docker Hub only |
| `DOCKER_HOST`, `DOCKER_CERT_PATH`, `DOCKER_TLS_VERIFY`, `DOCKER_CONFIG` | used as they are |
| `WATCHTOWER_NO_PULL` | label the containers `dockpaum.no-pull=true` |
| `WATCHTOWER_REVIVE_STOPPED` | not supported |

Without `WATCHTOWER_POLL_INTERVAL` or `WATCHTOWER_SCHEDULE`, dockpaum keeps its own 5-minute interval; Watchtower checked once a day.

## What carries over

- Labels: `com.centurylinklabs.watchtower.enable`, `monitor-only`, `no-pull`, `scope` and the `lifecycle.pre-update`/`post-update` hooks work as they are.
- Notification URLs (`discord://`, `slack://`, `ntfy://`, `smtp://` and the others) work in `WATCHTOWER_NOTIFICATION_URL`, `DOCKPAUM_NOTIFY_URLS` or `notify[].url`. Teams needs a Workflows webhook.
- Watchtower's `/v1/update` and `/v1/metrics` keep working for scripts and dashboards such as Homepage, on port 8080 as before.

## What changes

- A container that doesn't start or isn't healthy within 2 minutes after an update is rolled back.
- Stopped containers are updated but stay stopped.
- `REPO_USER` and `REPO_PASS` only apply to Docker Hub.
- dockpaum keeps an audit log in `./data`: `docker exec dockpaum /dockpaum history`.

To move to a config file later, start from [examples/watchtower.yml](examples/watchtower.yml), put it in `./config/dockpaum.yml`, and remove the `WATCHTOWER_*` variables.
