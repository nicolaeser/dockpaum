# dockpaum

dockpaum keeps your Docker containers up to date, like Watchtower. It adds update policies by version range, schedules and maintenance windows, a soak time for new images, an approval gate, health checks with automatic rollback, hooks, notifications, an audit log and Prometheus metrics.

## Quick start

```sh
mkdir dockpaum && cd dockpaum
curl -fsSLO https://raw.githubusercontent.com/nicolaeser/dockpaum/main/docker-compose.yml && docker compose up -d
```

dockpaum now checks your running containers every 5 minutes and updates each one when the image behind its tag changes. A container that doesn't start or doesn't become healthy after an update is rolled back. Its config is in `./config/dockpaum.yml`, its audit log and state in `./data`. See what it would do next:

```sh
docker exec dockpaum /dockpaum plan
```

Without Compose or a config file, like Watchtower:

```sh
docker run -d --name dockpaum --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/nicolaeser/dockpaum:latest
```

## Documentation

- [Install](docs/install.md): Compose, `docker run`, image tags, upgrades, several projects on one host, systemd.
- [Configuration](docs/configuration.md): the config file, environment variables, labels and every setting.
- [HTTP API and metrics](docs/http-api.md), with the full route reference in [openapi.json](docs/openapi.json).
- [Migrating from Watchtower](docs/migrating-from-watchtower.md)
- [Troubleshooting](docs/troubleshooting.md)

## Development

```sh
make build              # bin/dockpaum
make test               # tests with the race detector
make lint               # staticcheck and golangci-lint
make check              # formatting, vet, lint, tests, govulncheck, go mod tidy
make schema             # regenerate dockpaum.schema.json after changing the config model
make docker             # build the image for this machine
make docker-multiarch   # build the image for all platforms
```

Branch off `development` and open pull requests into `development`; `development` is merged into `main` through a pull request. GitHub Actions runs the checks on pull requests into and pushes to `development` and `main`, and publishes images from those two branches. `.gitlab-ci.yml` does the same on GitLab and pushes to ghcr.io with the CI/CD variables `GHCR_USER` and `GHCR_TOKEN` (a token with `write:packages`).
