# Configuration

dockpaum works without any configuration. You can change it in three places: the config file, environment variables, and labels on your containers.

## The config file

On first start dockpaum writes this default to `/etc/dockpaum/dockpaum.yml` (`./config/dockpaum.yml` with the compose file):

```yaml
dry_run: false

schedule:
  interval: 5m

update:
  policies:
    - id: default
      mode: tag-follow
      min_image_age: 0s
```

Keys you leave out take the defaults listed below. Note that a policy you write yourself without `min_image_age` waits one hour, unlike the built-in `0s`.

The file belongs to root with mode 0600, so edit it with `sudo`. Then check it and restart:

```sh
docker compose run --rm -T dockpaum validate --strict
docker compose restart
```

`validate` exits with 0 when the file is valid and 2 when it isn't; with `--strict` it exits with 3 on warnings. `docker exec dockpaum /dockpaum print-config` prints the settings in effect, with secrets masked.

[examples/](examples/) has configs to start from, such as `observe.yml` (report only), `homelab.yml` (nightly updates after a day's soak), `production.yml` (opt-in labels, patch releases, a maintenance window) and `watchtower.yml` (Watchtower's behavior), plus examples for windows, notifications, hooks, approval and CI triggers. Keep the first line `# yaml-language-server: $schema=https://raw.githubusercontent.com/nicolaeser/dockpaum/main/dockpaum.schema.json` for completion in your editor.

## Environment variables

They override the config file. Booleans accept `true`/`false`, `1`/`0`, `yes`/`no` and `on`/`off`.

| Variable | Sets |
|---|---|
| `DOCKPAUM_CONFIG` | Path of the config file (default `/etc/dockpaum/dockpaum.yml`). |
| `DOCKPAUM_DRY_RUN` | `dry_run` |
| `DOCKPAUM_SCHEDULE` | An interval (`6h`) or a cron spec (`0 4 * * *`). |
| `DOCKPAUM_TIMEZONE` | `timezone` (`TZ` works too) |
| `DOCKPAUM_LOG_LEVEL`, `DOCKPAUM_LOG_FORMAT` | `log.level`, `log.format` |
| `DOCKPAUM_HTTP_ENABLED`, `DOCKPAUM_HTTP_PORT`, `DOCKPAUM_HTTP_UPDATE_API` | `http.enabled`, `http.port`, `http.update_api` |
| `DOCKPAUM_HTTP_TOKEN`, `DOCKPAUM_HTTP_READ_TOKEN` (or with `_FILE`) | `http.token`, `http.read_token` |
| `DOCKPAUM_NOTIFY_URLS` (or `_FILE`) | Adds a channel per [notification URL](#notifications), separated by spaces, newlines or commas. |
| `DOCKPAUM_NOTIFY_EVENTS` | `events` of every channel that sets none. |
| `DOCKPAUM_LABEL_ENABLE`, `DOCKPAUM_SCOPE`, `DOCKPAUM_INCLUDE_STOPPED` | `filters.label_enable`, `filters.scope`, `filters.include_stopped` |
| `DOCKPAUM_CLEANUP`, `DOCKPAUM_ROLLBACK`, `DOCKPAUM_SELF_UPDATE` | `update.cleanup_images`, `update.rollback_on_failure`, `update.self_update.enabled` |
| `DOCKPAUM_MODE`, `DOCKPAUM_MIN_IMAGE_AGE`, `DOCKPAUM_HEALTH_TIMEOUT` | The same settings of the default policy. |
| `DOCKPAUM_INSTANCE`, `DOCKPAUM_STATE_DIR` | `instance`, `state.dir` |
| `DOCKPAUM_WATCHTOWER_ENV` | `true` reads `WATCHTOWER_*` variables, see [Migrating from Watchtower](migrating-from-watchtower.md). |
| `DOCKPAUM_CONFIG_URL` | Load the config from an HTTPS URL instead of the file. `DOCKPAUM_INSECURE_CONFIG_URL=true` allows `http://`. |

`DOCKER_HOST`, `DOCKER_CERT_PATH` and `DOCKER_TLS_VERIFY` set up the default Docker host, and `DOCKER_CONFIG` names a directory with a `config.json` for registry logins.

## How dockpaum decides

Each run takes the containers that pass the [filters](#settings), finds each one's [policy](#policies), and stops at the first gate that applies. The outcome goes into the [audit log](#audit-log):

1. `dockpaum.policy=none`, an unknown policy, or `mode: none`: `gated-policy`.
2. Swarm, Kubernetes or Nomad manage it, it runs with `--rm`, or it has its own MAC address on Docker Engine older than 25: `gated-orchestrated`, `gated-autoremove`, `gated-mac-address`.
3. The image is pinned by digest: `gated-digest-pin`.
4. The policy's maintenance window is closed: `gated-window`. dockpaum still checks for a newer image where it can.
5. Nothing newer in the registry: `skipped`. The registry can't be asked: `check-failed`.
6. The new image is younger than `min_image_age`: `gated-min-age`.
7. The container is monitor-only: `monitor-only`.
8. `dry_run` is on: `dry-run`.
9. The approval endpoint says no: `approval-denied`.

Then dockpaum pulls the image and runs the `pre_update` hooks. It stops the old container and renames it `<name>.dockpaum-old-<n>`, then creates the new one with the same volumes, networks, ports, labels and environment; defaults that came from the old image are replaced by the new image's. It waits up to `health_timeout` for the new container to be healthy. Then it removes the old container (keeping its volumes) and runs the `post_update` hooks. If the new container fails to start or become healthy, the old one comes back (`rolled-back`). A leftover from an interrupted update is restored by the next run (`recovered`).

`docker exec dockpaum /dockpaum explain <container>` walks one container through these gates.

## Syntax

- Unknown keys are errors, with a hint for the key you probably meant.
- `${VAR}` reads the container's environment, with `${VAR:-default}` and `${VAR:?message}` as in Compose. `$$` is a literal `$`. `${DOCKPAUM_*}` is left for hooks.
- Durations: `30s`, `5m`, `1h30m`, `1d`, `2w`. A plain number is seconds.
- Cron: five fields (`0 4 * * *`), six with seconds first, or `@daily`, `@every 6h`. `CRON_TZ=Europe/Berlin 0 4 * * *` sets the time zone.
- Maintenance windows: a list of `days` (`mon`-`sun`, `Mon-Fri`, `weekdays`, `weekends`; empty is every day) and `times` (`HH:MM-HH:MM`, may cross midnight; empty is all day).
- A single value works where a list is expected. `schedule: 6h` and `schedule: "0 4 * * *"` are short forms.
- Any key ending in `_file` reads its value from a file.

## Settings

| Key | Default | Meaning |
|---|---|---|
| `dry_run` | `false` | Check and report only: nothing is pulled or changed, no hooks or jobs run. |
| `timezone` | `TZ`, else UTC | Time zone of schedules, windows, policies and jobs. |
| `instance` | host name | Name of this dockpaum in notifications and the API. |
| `schedule.interval` | `5m` | Time between runs. |
| `schedule.cron` | | A cron spec instead of `interval`. |
| `schedule.timezone` | `timezone` | |
| `schedule.run_on_start` | with `interval` | Also run at startup. With `cron` only in dry-run mode. |
| `schedule.windows` | | Scheduled runs only start inside these. Runs you request ignore them. |
| `filters.label_enable` | `false` | Only manage containers labelled `dockpaum.enable=true`. |
| `filters.include_stopped` | `false` | Also update stopped containers (they stay stopped). |
| `filters.include_names`, `filters.exclude_names` | | Container names. |
| `filters.include_regex`, `filters.exclude_regex` | | RE2 expressions on the name. |
| `filters.scope` | | Only containers labelled `dockpaum.scope=<scope>`. Without it, scoped containers are left to their own instance. |
| `filters.watchtower_labels` | `true` | Honor Watchtower's labels. |
| `update.strategy` | `rolling` | `rolling` (one at a time) or `parallel`. |
| `update.concurrency` | `1` | Updates at once with `parallel`. |
| `update.stop_timeout` | `30s` | Time to stop before Docker kills the old container. |
| `update.rollback_on_failure` | `true` | Bring the old container back when the new one fails. |
| `update.cleanup_images` | `false` | Remove the old image when nothing uses it. |
| `update.restart_unchanged` | `false` | Recreate containers on every run, even without a new image. |
| `update.compose_group` | `false` | Update a Compose project together, in `depends_on` order. |
| `update.self_update.enabled` | `false` | Let dockpaum update its own container. |
| `update.self_update.delay`, `update.self_update.helper_image` | `5s`, the new image | The helper container that replaces dockpaum. |
| `log.level`, `log.format` | `info`, `json` | `debug` logs every gate decision. Formats: `json`, `text`. |
| `audit.enabled`, `audit.dir`, `audit.retention` | `true`, `/var/lib/dockpaum/audit`, `31d` | One JSON file per month. |
| `state.enabled`, `state.dir` | `true`, `/var/lib/dockpaum/state` | |
| `docker_config` | | A Docker `config.json` whose `auths` are used for registries. |
| `http.*` | | See [HTTP API](http-api.md). |

## Policies

A container uses the policy its `dockpaum.policy` label names, or the `default` policy.

```yaml
update:
  policies:
    - id: default
      mode: tag-follow
      min_image_age: 6h
    - id: database
      mode: minor
      schedule: "30 3 * * SUN"
      health_timeout: 10m
      hooks:
        pre_update: ["exec:@self:pg_dumpall -U postgres -f /backup/pre-update.sql"]
```

| Key | Default | Meaning |
|---|---|---|
| `id` | | Name used by the label. `none` and `off` are reserved. |
| `mode` | `tag-follow` | See below. |
| `min_image_age` | `1h` | Soak time before a new image is used. `0s` turns it off. |
| `min_image_age_source` | `auto` | `auto` (build time, or first seen if that's unusable), `first_seen` or `created`. |
| `health_timeout` | `2m` | Time to become healthy before rollback. `0s` turns the check off. |
| `monitor_only` | `false` | Report updates, never apply them. |
| `schedule` | | Own cron. The policy's containers then update only then. |
| `windows`, `timezone` | | Maintenance windows and their time zone. |
| `tags.include`, `tags.exclude` | | RE2 expressions for candidate version tags. |
| `tags.transform` | | Turns a tag into a version: `"^release-(.+)$ => $1"`. |
| `hooks.pre_update`, `post_update`, `on_failure` | | Commands after the global hooks. |

Modes: `tag-follow` keeps the tag and updates when its image changes (`digest` is the same). `patch` goes to the newest `x.y.*` and needs three-part tags. `minor` goes to the newest `x.*`. `major` goes to the newest version. `none` never checks. The version modes keep the tag's form (`v` prefix, number of parts, suffix such as `-alpine`) and skip pre-releases. A tag like `latest` is followed as in `tag-follow`.

Labels can also name the built-in policies `tag-follow`, `patch`, `minor`, `major` and `monitor`, which take their soak time, health timeout and windows from the default policy. An unknown policy name stops the container instead of falling back.

## Labels

Set these on the containers dockpaum manages. In a compose file, write `$$` for each `$`.

| Label | Effect |
|---|---|
| `dockpaum.enable` | `false`: never touch the container. With `filters.label_enable`, only `true` containers are managed. |
| `dockpaum.policy` | Policy id, a built-in policy, or `none`/`off`. |
| `dockpaum.monitor-only` | `true`: report updates, never apply them. |
| `dockpaum.no-pull` | `true`: never pull; update when the local image behind the tag changes. |
| `dockpaum.scope` | Belongs to the instance with this `filters.scope`. |
| `dockpaum.compose-group` | Update together with `update.compose_group`. |
| `dockpaum.hook.pre-update`, `post-update`, `on-failure` | Commands run inside the container, with `hooks.label_hooks: true`. |
| `dockpaum.link.template` | Release notes link, with `${tag}`, `${version}`, `${major}`, `${minor}`, `${patch}`. |
| `dockpaum.mode`, `dockpaum.min-image-age`, `dockpaum.min-image-age-source`, `dockpaum.health-timeout`, `dockpaum.tag.include`, `dockpaum.tag.exclude`, `dockpaum.tag.transform` | Override that policy setting for this container. An invalid value skips the container. |

Watchtower's `enable`, `monitor-only`, `no-pull`, `scope`, `lifecycle.pre-update` and `lifecycle.post-update` labels work as their dockpaum counterparts; a dockpaum label wins. Its other labels have no effect.

## Hooks and jobs

```yaml
hooks:
  timeout: 2m
  pre_run: ["/etc/dockpaum/snapshot.sh"]
  pre_update: ["exec:@self:/app/bin/drain --timeout 30s"]
  on_failure: ["/etc/dockpaum/page.sh"]

jobs:
  - name: nightly-backup
    cron: "30 2 * * *"
    commands: ["exec:postgres:pg_dumpall -U postgres -f /backup/all.sql"]
```

A command runs with `sh -c` inside the dockpaum container, where `./config` is `/etc/dockpaum` (on the host with systemd), or with `exec:<container>:<command>` inside that container. `exec:@self:` is the container being updated.

- `pre_run`, `post_run`: before and after each run. A failing `pre_run` aborts the run. No `exec:`.
- `pre_update`: a failure stops the update; exit code 75 skips it.
- `post_update`, `on_failure`: after a successful or failed update.
- `timeout` (`60s`), `disabled`, and `label_hooks` (run the label hooks above).
- Jobs have `name`, `cron`, `timezone`, `commands`, `host` (for `exec:` with several hosts) and `timeout`. They ignore windows.

Global hooks run before policy hooks, then label hooks. Nothing runs in dry-run mode. Hooks get `DOCKPAUM_PHASE`, `DOCKPAUM_RUN_ID`, `DOCKPAUM_POLICY`, `DOCKPAUM_TARGET`, `DOCKPAUM_HOST`, `DOCKPAUM_IMAGE` and `DOCKPAUM_NEW_IMAGE`; `post_run` also gets `DOCKPAUM_SCANNED`, `DOCKPAUM_UPDATED` and `DOCKPAUM_FAILED`, jobs `DOCKPAUM_JOB`. Write `$$HOME` for variables the shell should expand.

## Hosts

Without `hosts`, dockpaum manages the local Docker daemon.

```yaml
hosts:
  - name: local
  - name: build-1
    endpoint: tcp://build-1.example.com:2376
    tls:
      ca_cert: /etc/dockpaum/build-1/ca.pem
      client_cert: /etc/dockpaum/build-1/cert.pem
      client_key: /etc/dockpaum/build-1/key.pem
```

`endpoint` is `unix:///path` or `tcp://`; `ssh://` isn't supported. `tls.verify` defaults to `true`. An unreachable host fails the run, but the others are still updated.

## Registries

Public images need nothing. For a private registry, put the token in a file next to the config, such as `./config/ghcr.pull-token`:

```yaml
registry:
  - server: ghcr.io
    username: my-user
    password_file: /etc/dockpaum/ghcr.pull-token
```

`server` can include a repository path (`ghcr.io/acme/project-a`) to limit the entry to it; the longest match wins. `password` works too, ideally as `${VAR}`. `password_file` needs a `username` and is read on every use. If it is missing or empty, the check fails (`check-failed`) instead of pulling without credentials. A project can also bring its own token in a `.pull_token` file, which wins over these entries; see [Several projects on one host](install.md#several-projects-on-one-host).

## Notifications

```yaml
notify:
  - type: slack
    url: ${SLACK_WEBHOOK_URL}
    events: [update.failure, run.completed]
  - url: ntfy://ntfy.sh/my-topic
```

`url` can be a notification URL as Watchtower uses them: `discord://`, `slack://`, `telegram://`, `ntfy://`, `gotify://`, `teams://`, `smtp://`, `pushover://`, `matrix://`, `mattermost://`, `apprise://` or `generic+https://`. Otherwise set `type` and its fields:

| `type` | Fields |
|---|---|
| `slack`, `discord`, `teams` | `url` (webhook; Teams needs a Workflows webhook), Slack also `channel` |
| `telegram` | `token`, `channel` (chat ID) |
| `ntfy` | `url` (topic URL), optional `token` |
| `gotify` | `url`, `token` |
| `pushover` | `token`, `user`, optional `channel` (devices) |
| `matrix` | `url` (homeserver), `token`, `room` |
| `apprise` | `url` (Apprise API), optional `to`, `channel` (tags) |
| `email` | `smtp.host`, `to`, optional `from`, `smtp.port` (587), `smtp.username`, `smtp.password`, `smtp.starttls` |
| `webhook` | `url`, optional `headers`, `token`; posts JSON |

Every channel takes `name`, `events` and `once`, and secrets from files with `url_file`, `token_file`, `header_files` and `smtp.password_file`.

Without `events`, a channel gets `update.success`, `update.failure` and `run.completed` (only when something happened). `*` sends everything. The other events are `update.available` (monitor-only, dry-run or a check found an update), `update.skipped` (a gate held it back), `update.denied`, `update.rolled_back`, `update.recovered`, `check.failed`, `self_update.started`, `self_update.success`, `self_update.failure`, `run.started`, `dockpaum.started` and `dockpaum.stopping`. A waiting update is reported once until it changes; `once: false` repeats it every run. Test with `docker exec dockpaum /dockpaum notify test`.

## Audit log

`docker exec dockpaum /dockpaum history` reads it (`--since 7d`, `--container web`, `--json`). Each line has the container, image, tags, digests, policy, `decision` and `reason`.

| `decision` | Meaning |
|---|---|
| `updated` | Recreated with the new image. |
| `rolled-back` | The new container failed; the old one runs again. |
| `failed` | The update failed. |
| `check-failed` | The registry couldn't be asked or the image pulled. Nothing changed. |
| `check-incomplete` | No other tag of the current tag's form exists, so dockpaum can't tell. |
| `skipped` | Nothing newer, or a `pre_update` hook asked to skip. |
| `dry-run`, `monitor-only` | An update is available but not applied. |
| `gated-*`, `approval-denied` | Stopped by a gate, see [How dockpaum decides](#how-dockpaum-decides). `gated-policy` also covers dockpaum's own container while self-update is off. |
| `reattached` | Restarted to rejoin the network of a replaced container. |
| `recovered` | A leftover of an interrupted update was restored. |
| `self-update`, `self-update-completed`, `self-update-failed` | dockpaum's own update. |

## Approval gate

dockpaum can ask an HTTP endpoint before each update. dry-run mode never asks.

```yaml
update:
  approval:
    url: https://approvals.example.com/dockpaum
    header_files:
      Authorization: /etc/dockpaum/approval-token
    recheck_before_stop: true
    signing:
      secret_file: /etc/dockpaum/approval-signing-key
```

| Key | Default | Meaning |
|---|---|---|
| `url` | | The endpoint. |
| `method` | `POST` | `POST`, `PUT`, `PATCH` send JSON; `GET`, `HEAD`, `DELETE` send query parameters. |
| `headers`, `header_files` | | Extra headers. |
| `timeout` | `10s` | |
| `ok_statuses` | any 2xx | Statuses that approve. |
| `approve_on` | `success` | `failure` reads the answer the other way round: a 2xx denies, anything else approves. |
| `fail_open` | `false` | Approve when the endpoint can't be reached. |
| `label_allowlist` | | Labels sent besides `dockpaum.*` and `com.docker.compose.*`; `*` sends all. |
| `recheck_before_stop` | `false` | Ask again right before the old container is stopped. |
| `signing.secret_file` | | Sign every request with this HMAC secret (at least 32 bytes). |
| `signing.key_id` | `default` | Key ID sent with signed requests. |

The request carries `request_id`, `phase` (`initial` or `final`), `host`, `container`, `container_id`, `image`, the old and new digests, `labels` and `at`. A 2xx answer approves unless its JSON body says `"approved": false`. The body can add a `reason` and `retry_after_seconds` (or a `Retry-After` header) before which dockpaum doesn't ask again, at most 7 days.

A signed request carries `X-Dockpaum-Key-Id`, `X-Dockpaum-Timestamp`, `X-Dockpaum-Nonce` and `X-Dockpaum-Signature: v1=<hex>`: the lower-case hex HMAC-SHA256 of these lines joined by `\n`:

```text
DOCKPAUM-APPROVAL-V1
<method, upper case>
<request URI: path, and "?" plus the raw query if any>
<Host header, lower case, without the default port>
<timestamp>
<nonce>
<lower-case hex SHA-256 of the body>
```

Signed requests follow no redirects, and a secret that can't be read denies the update. Approvers should reject timestamps more than 300 seconds off and repeated nonces. Test yours against [approval-v1-vector.json](approval-v1-vector.json).
