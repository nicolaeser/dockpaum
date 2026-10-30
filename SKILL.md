---
name: dockpaum
description: Writes, reviews and validates configuration for dockpaum, a Docker container auto-updater like Watchtower. Covers dockpaum.yml, update policies, maintenance windows and soak time, dockpaum.* container labels, per-project .pull_token registry credentials, hooks and jobs, notifications, the HTTP API and signed external approval endpoints. Use when a task mentions dockpaum, dockpaum.yml, dockpaum.* labels, .pull_token, an update approval endpoint or the dockpaum validate command.
compatibility: Validating a config needs the dockpaum binary or its container image (dockpaum validate --strict).
---

# dockpaum

dockpaum keeps Docker containers up to date. Each run it takes the containers that pass the filters, finds each one's policy, and applies the first gate that holds the update back: policy mode, maintenance window, soak time, monitor-only, dry run, approval endpoint. It then pulls the image, recreates the container with the same settings and rolls back when the new one doesn't become healthy.

Without a config file dockpaum works like Watchtower: every 5 minutes it updates every container whose image changed, and it leaves its own container alone. The sources of truth are:

- `dockpaum.schema.json`: every key with type and description (`dockpaum schema` prints it).
- `docs/examples/*.yml`: tested configs to start from.
- `docs/configuration.md`: keys, defaults, labels, hooks, approval protocol.
- `dockpaum validate --strict`: decides whether a config is right.

## Workflow

Copy this checklist and keep it updated:

```text
Task progress:
- [ ] Step 1: Find out which containers to manage, how their images are tagged, and who may set their labels.
- [ ] Step 2: Start from the closest file in docs/examples/ (see References), or from the default: dockpaum init > dockpaum.yml
- [ ] Step 3: Decide selection, policy mode, windows, soak time and approval (see Decision points).
- [ ] Step 4: Keep every secret in a file and reference it with a *_file key. Never write a secret into the YAML.
- [ ] Step 5: Add the labels (dockpaum.policy, dockpaum.enable) to the compose files.
- [ ] Step 6: Run: dockpaum validate --strict --config ./dockpaum.yml
- [ ] Step 7: Fix every error (exit 2) and warning (exit 3), then repeat Step 6 until it exits 0.
- [ ] Step 8: Run dockpaum print-config (no secret may show) and dockpaum plan (the decisions you expect).
- [ ] Step 9: Roll out with dry_run: true first, read the audit log (dockpaum history), then set dry_run: false.
```

With the compose file from the README, the config is `./config/dockpaum.yml`, owned by root. Check it inside the image that will run it:

```sh
docker compose run --rm -T dockpaum validate --strict
```

Exit codes: `0` valid, `2` invalid, `3` valid with warnings (only with `--strict`). dockpaum reads the file once at start: apply a change with `docker compose restart`.

## Config shape and precedence

One YAML file. Top-level keys:

- `dry_run`, `timezone`, `instance`.
- `schedule`: `interval` or `cron`, plus global `windows`.
- `filters`: which containers are managed.
- `update`: `strategy`, `rollback_on_failure`, `policies`, `approval`, `self_update`.
- `registry`, `docker_config`, `notify`, `hooks`, `jobs`.
- `http`, `audit`, `state`, `log`.
- `hosts`: only for Docker daemons besides the local socket.

Settings apply in this order:

1. The file: `--config`, else `$DOCKPAUM_CONFIG`, else `/etc/dockpaum/dockpaum.yml`. `DOCKPAUM_CONFIG_URL` (HTTPS) loads it remotely instead. A missing file is replaced by the default on first start.
2. `${VAR}` is expanded from the environment, then the `*_file` secrets are read.
3. `DOCKPAUM_*` variables override the file. With `DOCKPAUM_WATCHTOWER_ENV=true`, `WATCHTOWER_*` variables are read first.
4. Defaults fill the rest, then validation runs.
5. Per container, `dockpaum.policy` selects the policy and override labels adjust it. `dockpaum.*` labels win over the Watchtower labels dockpaum also understands.

## Decision points

**Selection**

- Default: every running container except those labelled `dockpaum.enable=false`.
- Shared host, or containers owned by others: set `filters.label_enable: true`. Only `dockpaum.enable=true` containers are then managed.
- Several dockpaum instances on one daemon: give each a `filters.scope` and label its containers `dockpaum.scope=<scope>`. Usually one instance per host is better.

**Policy mode** (per container with `dockpaum.policy=<id>`)

- `tag-follow` (default) updates when the image behind the tag changes. It suits `latest`, `stable`, `1`, `1.4`.
- `patch` follows `1.4.x` and needs three-part tags. `minor` follows `1.x` and suits databases (`postgres:16`). `major` follows everything: combine it with an approval endpoint.
- Image built on the host: label it `dockpaum.no-pull=true`.
- Report but never apply: `dockpaum.policy=monitor`, or `monitor_only: true` in the policy.
- Never touch: `dockpaum.policy=none` or `dockpaum.enable=false`.

**Soak time and health**

- A policy you write without `min_image_age` waits `1h`; the built-in default policy uses `0s`. Use days for third-party images.
- `health_timeout` defaults to `2m`. Rollback needs `rollback_on_failure` (default on).
- Updates only at certain times: `update.policies[].windows`. Skip whole runs outside a window: `schedule.windows`.
- A policy with its own `schedule` must fire inside its own `windows`, or it never updates.

**Approval endpoint**

- Default: none.
- An external system must agree first (change freeze, busy workload): set `update.approval` with an HTTPS `url`, `signing.secret_file` (at least 32 bytes) and `fail_open: false`. Add `recheck_before_stop: true` when the answer can change while the image is pulled.
- Update only when another system reports a problem: `approve_on: failure`. A 2xx then denies, any other status or an unreachable endpoint approves.
- Request, signature and answer format: the approval section of `docs/configuration.md`; test vector: `docs/approval-v1-vector.json`.

**Registry credentials**

- Public images need none.
- One token for everything: a `registry` entry with `password_file` pointing into the mounted `./config` directory.
- Different tokens per compose project: a `.pull_token` file next to that project's `docker-compose.yml`. One line per entry, `<registry>[/<namespace>[/<repo>]] <username> <token>`, `#` lines ignored:

  ```text
  ghcr.io/org-a  user-a  ghp_xxx
  ghcr.io/org-b  user-b  ghp_yyy
  ```

  dockpaum finds the file through the container's `com.docker.compose.project.working_dir` label and uses it for that project only. The most specific entry wins; equal scopes are tried in file order. It beats the `registry` entries, which stay the fallback. Use `chmod 600` and add it to `.gitignore`.

**Secrets**

- Use `*_file` keys, or `${VAR}` from the environment. Never write a literal secret into the config or the repository.

## Gotchas

- Cron has five fields, or six with seconds first. `schedule.interval` and `schedule.cron` are exclusive.
- Durations need a unit: `30s`, `5m`, `1h30m`, `2d`, `1w`. A plain number is seconds, but `1h30` fails.
- `$VAR` and `${VAR}` are expanded when the config loads, also inside hook commands. Write `$$VAR` for the shell. `DOCKPAUM_*` variables passed to hooks stay untouched. In compose files, write `$$` for each `$`.
- Unknown keys are errors. An unknown `dockpaum.policy` id holds the container back (`gated-policy`) instead of using the default policy.
- A `DOCKPAUM_*` variable in the container environment overrides the file. Check it when the config seems ignored.
- Secret files read at load must exist when validating (error, exit 2): `http.token_file`, `http.read_token_file`, `notify[].url_file`, `token_file`, `header_files`, `smtp.password_file`, `update.approval.header_files`. Read on use instead: `registry[].password_file`, `update.approval.signing.secret_file` (missing: `--strict` exits 3 and every update is denied).
- `dry_run: true` with `jobs` or `pre_run`/`post_run` hooks is a warning: they don't run in dry-run mode.
- `patch` needs three-part tags. Images pinned by digest (`@sha256:`) are never updated.
- Override labels (`dockpaum.mode`, `dockpaum.min-image-age`, `dockpaum.health-timeout`, `dockpaum.tag.*`) loosen a policy for one container. Whoever writes labels controls them.
- `hooks.label_hooks` runs commands from container labels inside those containers. Keep it off unless every label author is trusted.
- With `recheck_before_stop`, the `pre_update` hooks have already run when the final check denies, and no `post_update` or `on_failure` hook follows. Keep `pre_update` hooks safe to repeat.
- A 2xx answer with `"approved": false` denies. With `approve_on: failure`, `fail_open` has no effect.
- A container managed by Swarm, Kubernetes or Nomad, run with `--rm`, or with its own `--mac-address` on Docker Engine older than 25, is never replaced.
- With a token set, every `/internal-api/` route needs `Authorization: Bearer`, including health. `POST /internal-api/v1/update` and `/v1/check` also need `http.update_api: true` and the full token.

## Security checklist

- [ ] Docker socket access is root access. Remote daemons only over `tcp://` with mutual TLS (`tls.verify` stays `true`).
- [ ] No secret is written inline; `dockpaum print-config` shows none.
- [ ] `http.token` is set before the API is reachable from other machines. Scrapers get `read_token`.
- [ ] The approval endpoint uses HTTPS, signing and `fail_open: false`.
- [ ] `hooks.label_hooks` is off unless every label author is trusted. A remote config loads only over HTTPS.
- [ ] `rollback_on_failure` stays on, every policy has a `health_timeout`, fresh images get a soak time.
- [ ] `./data` holds audit and state; dockpaum itself runs from an immutable tag (see `docs/install.md`).

## References

- `docs/configuration.md`: every key, default, label, hook, notification and the approval protocol. Read when choosing keys or values.
- `docs/install.md`: Compose, image tags, upgrades, rollout to many hosts, several projects on one host, systemd. Read when deploying dockpaum itself.
- `docs/http-api.md` and `docs/openapi.json`: API setup, tokens, trigger routes, metrics. Read when exposing the API or triggering runs from CI.
- `docs/migrating-from-watchtower.md`: Watchtower variables, labels and flags. Read when replacing Watchtower.
- `docs/troubleshooting.md`: common errors and what they mean. Read when validation or a run fails.
- `docs/examples/basic.yml`: live updates every 5 minutes, tag-follow with a 1h soak. The smallest complete setup.
- `docs/examples/cron-and-windows.yml`: cron inside global and policy windows, a weekly slot. Read when scheduling updates.
- `docs/examples/with-notifications-hooks.yml`: Slack and Discord, run and update hooks, a database policy, a nightly job.
- `docs/examples/multi-host-approval.yml`: remote hosts over mutual TLS, approval, registry credentials.
- `docs/examples/external-approval-gate.yml`: a signed approval endpoint with `recheck_before_stop`.
- `docs/examples/ci-triggered.yml`: runs queued from CI through the update API.
- `docs/examples/homelab.yml`, `production.yml`, `observe.yml`: nightly updates, opt-in patch-only updates in a window, report only.
- `docs/examples/watchtower.yml`, `watchtower-migration.yml`: Watchtower's behavior and API; a Watchtower setup translated.
