# Troubleshooting

Start here:

```sh
docker logs dockpaum                          # errors and warnings
docker exec dockpaum /dockpaum explain web    # why "web" is or isn't updated
docker exec dockpaum /dockpaum history        # recent decisions
```

`DOCKPAUM_LOG_LEVEL=debug` logs every filter and gate decision. The decisions are explained in [Configuration](configuration.md#audit-log).

## Docker socket: permission denied

The image runs as root and can use the socket. If you run it as another user (`user:` in the compose file), that user needs the socket's group: add `group_add: ["<gid>"]` with the number from `stat -c %g /var/run/docker.sock`. dockpaum logs the socket's owner and its own groups next to the error.

## Config errors

| Message | Fix |
|---|---|
| `schedul: unknown key — did you mean "schedule"?` | A typo or a key at the wrong level. |
| `"1h30" is not a duration` | Give every part a unit: `1h30m`. |
| `url is required (${SLACK_WEBHOOK_URL} is not set)` | Pass the variable to the container under `environment:`. |
| `${DOCKPAUM_HTTP_TOKEN} stays literal here` | `${DOCKPAUM_*}` is kept for hooks. Use another name, or set the `DOCKPAUM_*` variable directly. |
| `http.update_api: requires http.token` | Set a token. |
| `validate` says the file is `not present or empty` | It checked the built-in default because `/etc/dockpaum/dockpaum.yml` is missing. |

## Containers aren't updated

`dockpaum explain <container>` names the gate that stops it. The usual ones:

| Decision | Cause |
|---|---|
| `dry-run` | Observe mode is on. Set `dry_run: false` or `DOCKPAUM_DRY_RUN=false`. After a Watchtower migration, `DOCKPAUM_WATCHTOWER_ENV=true` keeps it on until you do. |
| `skipped` | Nothing newer. `patch` needs three-part tags: for `postgres:16.2` use `minor`. |
| `gated-min-age` | The new image is younger than `min_image_age` (one hour unless set). |
| `gated-window` | The maintenance window is closed. `validate` warns when a schedule never fires inside its windows. |
| `gated-policy` | `dockpaum.policy=none`, an unknown policy, an invalid override label, or `mode: none`. |
| `gated-orchestrated`, `gated-autoremove`, `gated-mac-address` | Managed by Swarm, Kubernetes or Nomad, started with `--rm`, or a fixed MAC address on Docker Engine older than 25. |
| `approval-denied` | The approval endpoint said no. |
| `check-incomplete` | No other tag of the current tag's form exists, for example after an image variant was renamed. |

A container that doesn't show up at all is excluded by `dockpaum.enable=false`, `filters.label_enable`, a scope or a name filter.

## Registry errors

`check-failed` means the registry couldn't be asked or the image pulled. Nothing changed, but the run counts as failed.

- `unauthorized`: credentials missing or wrong, a wrong or expired entry in a project's `.pull_token`, or a `password_file` that is missing or empty.
- Docker Hub rate limits show up here too. Check less often, or add Docker Hub credentials.
- An image built on the host isn't in any registry. Label the container `dockpaum.no-pull=true` or `dockpaum.enable=false`.

## Other problems

| Problem | Cause and fix |
|---|---|
| `rolled-back` | The new container didn't start or wasn't healthy within `health_timeout`. Check its logs and health check. |
| A container named `<name>.dockpaum-old-<n>` | An interrupted update. The next run restores it when the name is free; otherwise check the new container and remove the leftover. |
| `docker host(s) unreachable` | A host is down or its TLS settings are wrong. The other hosts are still updated. |
| dockpaum doesn't update itself | That's the default. Run `docker compose pull && docker compose up -d`, or set `update.self_update.enabled: true`. Pinned tags never change, and under systemd you run the installer again. |
| A hook sees an empty `$HOME` | `$VAR` is replaced when the config loads. Write `$$HOME`. |
| Label hooks don't run | Set `hooks.label_hooks: true`. They only run for running containers. |
| No notifications | Run `docker exec dockpaum /dockpaum notify test`. Without `events`, a channel only gets `update.success`, `update.failure` and `run.completed`. |
| `401` from an uptime monitor or probe | With a token set, health and readiness need it too. Send the read-only token. |
| `404` or `403` from `/internal-api/v1/update` or `/check` | Set `http.update_api: true` and use the full token. |
| `ignoring WATCHTOWER_...` in the log | Set `DOCKPAUM_WATCHTOWER_ENV=true`, see [Migrating from Watchtower](migrating-from-watchtower.md). |
