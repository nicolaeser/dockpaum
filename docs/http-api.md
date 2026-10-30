# HTTP API and metrics

dockpaum serves health checks, status, Prometheus metrics and two trigger routes on port 8080. This page covers setup; every route, parameter and response is in [openapi.json](openapi.json).

## Settings

| Key | Default | Meaning |
|---|---|---|
| `http.enabled` | `true` | `false` (or `DOCKPAUM_HTTP_ENABLED=false`, which wins over the file) means nothing listens. The container health check still passes. |
| `http.port` | `8080` | Port on all interfaces. |
| `http.addr` | | Listen address such as `127.0.0.1:8080`. Wins over `port`. |
| `http.token`, `http.token_file` | | Full token. |
| `http.read_token`, `http.read_token_file` | | Read-only token. Must differ from `token`. |
| `http.update_api` | `false` | Enable the trigger routes. Needs `token`. |
| `http.watchtower_compat` | `false` | Also serve Watchtower's `/v1/update` and `/v1/metrics`. |

Without a token, every read route is open to anyone who can reach the port. Once a token is set, every `/internal-api/` route needs `Authorization: Bearer <token>`, health, ready and busy included; the read-only token is enough for those. The image's health check sends the token itself, but uptime monitors and Kubernetes probes must send it too. The tokens can also come from `DOCKPAUM_HTTP_TOKEN` and `DOCKPAUM_HTTP_READ_TOKEN` (or their `_FILE` variants).

## Reach the API

The compose file doesn't publish the port. To look from the host:

```sh
docker exec dockpaum wget -qO- http://127.0.0.1:8080/internal-api/v1/summary
```

Add `--header "Authorization: Bearer <token>"` once a token is set. To reach the API from other machines, set a token, add `ports: ["8080:8080"]` to the compose file, and put a reverse proxy with TLS in front of it.

## Trigger an update or a check

```yaml
http:
  token: ${UPDATE_API_TOKEN}
  update_api: true
```

Both routes need `update_api` and the full token. Without them they answer 404; the read-only token gets 403. Both take `container` (repeatable), `image` (repeatable or comma-separated) and `policy`. Without any, they cover what a scheduled run covers.

`POST /internal-api/v1/update` queues an update run. It ignores `schedule.windows`, but every container still passes its policy's gates:

```sh
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" "http://docker-host:8080/internal-api/v1/update?container=api"
```

`POST /internal-api/v1/check` queues a run that only checks: no pulls, hooks or approval requests. The results show in `/internal-api/v1/containers` and as `update.available` notifications:

```sh
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" "http://docker-host:8080/internal-api/v1/check?policy=database"
```

`GET /internal-api/busy` returns `{"busy": true, "run_id": "...", "started_at": "..."}` while a run is going on and `{"busy": false}` otherwise.

## Metrics

Prometheus scrapes `/internal-api/metrics`:

```yaml
scrape_configs:
  - job_name: dockpaum
    metrics_path: /internal-api/metrics
    authorization:
      credentials: <read token>
    static_configs:
      - targets: ["docker-host:8080"]
```

The most useful metrics:

| Metric | Meaning |
|---|---|
| `dockpaum_run_failures_total` | Runs that failed. |
| `dockpaum_last_successful_run_timestamp_seconds` | When the last regular run finished without errors. |
| `dockpaum_container_update_available` | 1 while an update is waiting, per `host`, `container`, `policy`. |
| `dockpaum_container_updates_total`, `dockpaum_container_update_failures_total` | Updates and failed updates per container. |
| `dockpaum_container_update_skipped_total` | Updates held back, with the gate as `reason`. |
| `dockpaum_hosts_unreachable` | Docker hosts that couldn't be reached. |
| `dockpaum_registry_ratelimit_remaining` | Pulls left, as reported by the registry. |
| `dockpaum_notifier_failures_total` | Failed notifications per channel. |

`/internal-api/metrics` lists the rest. For alerts, `increase(dockpaum_run_failures_total[6h]) > 0` and `time() - dockpaum_last_successful_run_timestamp_seconds > 2 * 86400` are a good start.
