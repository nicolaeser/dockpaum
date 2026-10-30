# Install

## Docker Compose

```sh
mkdir dockpaum && cd dockpaum
curl -fsSLO https://raw.githubusercontent.com/nicolaeser/dockpaum/main/docker-compose.yml && docker compose up -d
```

The compose file runs `ghcr.io/nicolaeser/dockpaum:latest` as root, like Watchtower, and mounts:

- `/var/run/docker.sock`, so dockpaum can manage your containers. Anyone who controls dockpaum controls the host, so treat it like root.
- `./config` at `/etc/dockpaum`. On first start dockpaum writes its default config there as `dockpaum.yml`.
- `./data` at `/var/lib/dockpaum`, for the audit log and the state (sent notifications, when each image was first seen, image defaults). Without it dockpaum still works, but forgets all that on restart.

Out of the box dockpaum checks every running container right after it starts and then every 5 minutes, updates a container as soon as the image behind its tag changes, rolls it back when the new one doesn't start or isn't healthy within 2 minutes, and leaves its own container alone. Containers labelled `dockpaum.enable=false` (or Watchtower's `com.centurylinklabs.watchtower.enable=false`) are skipped.

To change that, edit `./config/dockpaum.yml`. It belongs to root with mode 0600, so use `sudo` or `sudo chown` it to yourself. Then check it and restart:

```sh
docker compose run --rm -T dockpaum validate --strict
docker compose restart
```

See [Configuration](configuration.md) for every setting. To see what dockpaum decided:

```sh
docker logs -f dockpaum
docker exec dockpaum /dockpaum plan                  # what the next run would do, and why
docker exec dockpaum /dockpaum history --since 24h   # what past runs did
```

## docker run

Without a config file, dockpaum behaves like Watchtower with a 5-minute interval. Add `-v "$PWD/data:/var/lib/dockpaum"` to keep the audit log and state, and `-e DOCKPAUM_...` variables to change settings.

```sh
docker run -d --name dockpaum --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/nicolaeser/dockpaum:latest
```

## Image tags

| Tag | Built from | Changes |
|---|---|---|
| `latest` | `main` | with every build |
| `v<YYYY.MM.DD>.<sha7>`, `<sha7>` | `main` | never |
| `dev` | `development` | with every build |
| `vdev<YYYY.MM.DD>.<sha7>`, `dev-<sha7>` | `development` | never |

Every tag is built for `linux/amd64`, `linux/arm64`, `linux/arm/v7` and `linux/arm/v6`.

## Upgrade and roll back

dockpaum doesn't update itself unless you turn that on. To upgrade it:

```sh
docker compose pull && docker compose up -d
```

With `update.self_update.enabled: true` (or `DOCKPAUM_SELF_UPDATE=true`), dockpaum updates its own container through a short-lived helper container, which brings the old one back if the new one isn't healthy.

In production, pin an immutable tag. You upgrade by changing it, and roll back by putting the previous one back and running `docker compose up -d`:

```yaml
services:
  dockpaum:
    image: ghcr.io/nicolaeser/dockpaum:v2026.10.30.1a2b3c4
```

`v2026.10.30.1a2b3c4` stands for the tag you want. `docker exec dockpaum /dockpaum version` shows the running version.

## Rolling out to many hosts

- Pin an immutable tag on every host, as above, and roll it out like any other change.
- Check that the image was built by this repository before you roll it out. Every pushed image is signed with [cosign](https://docs.sigstore.dev/) and carries an SBOM and build provenance:

  ```sh
  cosign verify ghcr.io/nicolaeser/dockpaum:v2026.10.30.1a2b3c4 \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp '^https://github.com/nicolaeser/dockpaum/\.github/workflows/ci\.yml@refs/heads/(main|development)$'
  ```

- Use the same `docker-compose.yml` and `config/dockpaum.yml` everywhere. Set what differs per host with `DOCKPAUM_*` variables, for example `DOCKPAUM_INSTANCE` for the name in notifications.
- Keep the HTTP port unpublished, or set `http.token` (and `http.read_token` for scrapers) before you publish it.
- Try a new config with `dry_run: true` and `docker exec dockpaum /dockpaum plan` on one host first.
- Send `update.failure` and `update.rolled_back` to a [notification](configuration.md#notifications) channel.

## Several projects on one host

Give each project's services a policy with labels in their own compose files (see [labels](configuration.md#labels)).

Most setups need no registry token, or one central `registry:` entry (see [Registries](configuration.md#registries)). But projects on the same registry often pull with different credentials: another user, or the same user with another token. For that, put a `.pull_token` file next to each project's `docker-compose.yml`. Its entries are only used for that project, so they never leak into another one:

```text
# ~/project-a/.pull_token: <registry>[/<namespace>[/<repo>]] <username> <token>
ghcr.io/org-a user-a ghp_xxx
```

```text
# ~/project-b/.pull_token
ghcr.io/org-b user-b ghp_yyy
```

dockpaum finds the file through the `com.docker.compose.project.working_dir` label of the project's containers. There is nothing to mount and nothing to restart: dockpaum reads the file on every check, through a short-lived helper container from its own image (directly under systemd). When the file is new or changed, it logs in once per entry and logs a wrong or expired token right away.

- One entry per line; lines starting with `#` are ignored. An image without a registry is on `docker.io`.
- Within a file, narrow entries by namespace or repository; the most specific one for an image wins. If the same scope appears more than once, dockpaum tries those entries in file order until the registry accepts one, and remembers which one worked.
- `.pull_token` wins over the `registry:` entries in `dockpaum.yml`, which stay the fallback.
- A symlink to a shared file works.
- Run `chmod 600 .pull_token`, and add it to the project's `.gitignore` and `.dockerignore`.

## systemd

You can run the binary as a systemd service instead. The host still needs Docker; the installer copies the binary out of `ghcr.io/nicolaeser/dockpaum:latest`, creates a `dockpaum` user in the `docker` group, installs `dockpaum.service`, and starts it:

```sh
curl -fsSL https://raw.githubusercontent.com/nicolaeser/dockpaum/main/deploy/systemd/install.sh | sudo sh
```

The config is `/etc/dockpaum/dockpaum.yml` (written on first start), values for `${VAR}` go in `/etc/dockpaum/dockpaum.env`, and logs in `journalctl -u dockpaum`. The service checks the config before every start. The HTTP API listens on port 8080 on every interface of the host; on a host reachable from untrusted networks, set `http.addr: 127.0.0.1:8080`, a token, or `http.enabled: false`.

To upgrade, run the installer again. To install your own build, run `sudo DOCKPAUM_BINARY=bin/dockpaum sh deploy/systemd/install.sh` from a checkout after `make build`. To remove dockpaum (`/etc/dockpaum` and `/var/lib/dockpaum` stay):

```sh
curl -fsSL https://raw.githubusercontent.com/nicolaeser/dockpaum/main/deploy/systemd/install.sh | sudo sh -s uninstall
```
