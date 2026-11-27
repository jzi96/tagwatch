# Tagwatch

A small Go service that watches public container registries and serves a plain,
server-rendered HTML report. No JavaScript, external assets, database server, or
automatic image updates.

## Features

- Explicit registry hosts: Microsoft Container Registry (`mcr.microsoft.com`),
  Google Container Registry (`gcr.io`), Google Artifact Registry
  (`LOCATION-docker.pkg.dev`), Azure Container Registry (`NAME.azurecr.io`),
  and other OCI/Docker Distribution registries with public tag listing and manifest access.
- Detect newly observed tags **and existing tags whose manifest/index digest changed**.
- Stable full SemVer tags by default (`1.2.3` and `v1.2.3`), sorted by version,
  not lexically. Optional prereleases and non-SemVer tags.
- Per-repository regex allow/deny filters. Deny takes precedence.
- Persistent JSON baselines and configurable change history (30 days by default).
- Optional inspection of running Docker containers through a Unix socket.
- Two-stage container build: `golang:1.27-alpine` → `alpine:3.24`.
  Only the compiled binary, certificates, and example config enter the runtime.
  The runtime runs as UID/GID `10001`, not root.

## Quick start with Docker Compose

```sh
cp config.example.yaml config.yaml
# Edit config.yaml to select repositories.
docker compose up -d --build
```

Open <http://localhost:8080>. The first successful check records a baseline; it
does **not** report every existing tag as new. Subsequent new tags and digest
changes appear in the report. With Docker enabled, a change already running in
every container of that repository is hidden, so applied updates disappear. With
no running container or configured version baseline, updates are determined
entirely by observations made since startup.

The named volume `tagwatch-data` preserves state across container replacements.
Compose first runs a one-shot initializer that gives the volume to UID/GID
`10001`; this also repairs volumes created by older or root-running versions.
Tagwatch itself still runs as the non-root user with all capabilities dropped.
Do not delete the volume unless you want to reset the baseline. Configuration is
read at startup; restart after editing it.

Without Compose:

```sh
docker build -t tagwatch:local .

# Repair an existing volume once. This is safe to rerun.


docker run -d --name tagwatch \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/config.yaml:/etc/tagwatch/config.yaml:ro" \
  -v tagwatch-data:/data:rw \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --replace \
  tagwatch:local
```

```sh
PODMAN_SOCKET=/run/user/$(id -u)/podman/podman.sock

podman run -d \
  --name tagwatch \
  --user 0:0 \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/config.yaml:/etc/tagwatch/config.yaml:ro" \
  -v tagwatch-podman-data:/data:rw \
  -v "$PODMAN_SOCKET:/var/run/docker.sock:ro" \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --replace \
  tagwatch:local
```

The embedded example config is used when no config is mounted. A new Docker
named volume inherits the image directory ownership. Older named volumes or
volumes populated by root must be repaired with the `chown` command shown
above before starting Tagwatch.

A host bind mount for `/data` must be writable by UID 10001, for example
`chown -R 10001:10001 ./tagwatch-data` before mounting it. If you explicitly
override the container user, make the bind mount or named volume writable by
that UID/GID instead. On SELinux hosts, configuration/data bind mounts may need
an appropriate label. Do not relabel the host Docker socket casually.

## Configuration

See [config.example.yaml](config.example.yaml) for all settings.

```yaml
listen: ":8080"
interval: 1h
timeout: 5m
state_file: /data/state.json
history_days: 30
scan_concurrency: 12
poll_concurrency: 3
docker_socket: ""

repositories:
  - image: stalwartlabs/stalwart
    semver_only: true
    include_prereleases: false
    allow: []
    deny: ['(?i)(nightly|pr[-_])']
    # baseline: "v0.11.0"
```

Repository names must not contain a tag, digest, or URL scheme. Hosts with ports
are supported. Duplicate normalized repositories, invalid regexes, unknown YAML
fields, and invalid durations are rejected at startup.

- `interval`: delay **after** a complete poll, minimum 1 second.
- `timeout`: total budget for each repository, and separately for the Docker
  inspection/comparison phase. Docker is inspected once per poll, before the
  repository checks start.
- `scan_concurrency`: manifest requests per repository at the same time, 1–64,
  default 12. One request per selected tag is what makes a check expensive.
- `poll_concurrency`: repositories checked at the same time, 1–16, default 3.
  Requests in flight are `scan_concurrency * poll_concurrency`.
- `scope`: `all` (default) resolves every selected tag; `pending` carries tags
  already known and at or below the oldest running version forward from the state
  file instead of requesting them. Unknown tags are always requested, and a tag
  equal to a running tag is still requested because its content must be compared.
  `pending` needs the Docker socket; without running containers nothing is skipped.
- `history_days`: 1–3650. History does not disappear on the next unchanged poll.
- `allow` / `deny`: Go regular expressions (RE2), matched against the complete
  original tag string, but **not implicitly anchored**. Use `^` and `$` for exact
  matching. Any deny match rejects; a nonempty allow list requires at least one match.
  The SemVer/prerelease policy is applied as well.
- `semver_only`: defaults to true. Full three-component versions are required;
  floating `1`, `1.2`, and `latest` tags are excluded unless false.
- `include_prereleases`: defaults to false. SemVer suffixes such as `-rc.1`,
  `-alpine`, and `-bookworm` are all prereleases under SemVer rules. If enabled,
  use allow/deny filters to select your desired release/variant channel.
- `baseline`: optional full SemVer tag used for a persistent “newer than” list,
  even without Docker and on the first check. It is a version threshold, not a
  previous digest, and need not currently exist upstream.

To include `latest`:

```yaml
repositories:
  - image: alpine
    semver_only: false
    allow: ['^latest$', '^v?[0-9]+\.[0-9]+\.[0-9]+$']
```

SemVer precedence ignores build metadata. There is no guessed version ordering
for arbitrary strings or floating aliases. New tags published on an older
release branch still appear in observed history.

## Compare against running Docker containers

Enable in `config.yaml`:

```yaml
docker_socket: /var/run/docker.sock
```

Then grant the non-root process the socket's numeric group:

```sh
export DOCKER_GID="$(stat -c '%g' /var/run/docker.sock)"
docker compose -f compose.yaml -f compose.docker.yaml up -d --build
```

Only **running** containers belonging to configured repositories are shown.
Each container gets its own comparison, so multiple running versions are not
collapsed into one baseline. The application never pulls layers, restarts
containers, or deploys anything.

- A running full-SemVer tag provides a version threshold. All currently available,
  filter-matching versions greater than it are listed, including on the first poll.
- The local image is inspected by immutable image ID. Its config digest is compared
  with the current tag's config digest for that container's OS/architecture/variant.
  This detects a tag rebuilt while the old image is still running, without confusing
  Docker image IDs with manifest or multi-platform index digests.
- “Matches running image content” means the **platform's image config** matches.
  It does not assert that the top-level multi-platform index is unchanged. Global
  digest history independently records those changes, including changes to other
  platforms or manifest metadata.
- `latest` and other non-SemVer tags can be checked for changed platform content,
  but cannot provide semantic version ordering. Digest-pinned containers are shown
  without guessing their release version.
- Observed history becomes a **pending list** while containers run: an event is hidden
  once every running container of that repository already provides the tag — either the
  identical tag whose platform image content still matches, or a newer SemVer version.
  A container lagging behind (for example a canary on the previous version) keeps the
  event visible, and a container whose tag was rebuilt upstream keeps it too. Without
  Docker, or with no container for the repository, history remains an observation log.
- Version candidates come from the last successful registry check. Failed checks
  are visibly marked as stale; socket failures are also visible and do not erase
  registry history. If a running tag disappears upstream, comparison shows an error.
- This is optional Unix-socket Docker Engine support, not Kubernetes/containerd
  discovery or remote TCP Docker access. Rootless Docker can use a different
  mounted socket path. Runtime access is opt-in even if a socket happens to exist.

**Security:** A Docker socket generally grants host-equivalent control. A `:ro`
bind mount does **not** make its HTTP API read-only. Tagwatch issues only GET
inspection requests, but a compromised process would still possess socket
access. Prefer a restricted socket proxy that exposes only container listing
and image inspection, presented through a Unix socket. The included override
mounts the socket directly for convenience; it does not provide API isolation.

The web service has no login or TLS and exposes image/container names. Compose
binds it to localhost by default. Use a trusted network or authenticated HTTPS
reverse proxy for remote access.

## What counts as an update?

1. List all tags and apply configured filters.
2. Resolve a manifest/index digest for every selected tag, without downloading layers.
3. On the first successful check, save the baseline without generating events.
4. On subsequent checks, record newly observed tags and changed digests.
5. Persist the complete snapshot atomically before publishing the new report.

A repository check is all-or-nothing: tag-list, timeout, or individual manifest
errors retain the last successful snapshot. A manifest request that fails is
retried twice (250 ms, then 1 s) before the repository check is considered
failed, so a single transient error no longer discards a complete scan. Other
repositories may still advance.
A failed state write advances neither the on-disk nor in-memory baseline and
is prominently reported. An invalid existing state file stops startup rather
than silently discarding history. Run only one instance per state file/volume.

Deleted tags are removed from the current snapshot, not reported as updates.
A deleted tag that reappears later is newly observed again. Changing filters can
likewise make previously untracked tags newly observed. Removed repositories
are discarded from state at the next successful state write.

History is time-limited, not count-limited. Expired events are hidden on report
generation and removed from a repository's persisted history at its next successful
check. The report shows one row per tag: repeated digest changes of the same tag,
such as a mutable tag rebuilt by the publisher, are collapsed into the most recently
observed one, while the state file keeps the full chain for auditing. Large
repositories and frequent digest changes can still produce sizeable state files;
narrow filters and choose polling intervals responsibly.

### Why not modified dates?

The standard OCI tag-list response contains names, not push timestamps.
Docker Hub's vendor API exposes `last_updated` and `tag_last_pushed`, but these
fields are not portable across registries. Image creation timestamps are build
metadata, not reliable tag-update timestamps.

Tagwatch therefore uses digests as the change signal and displays its **own
observation times**, not vendor timestamps. It does not fetch optional vendor
timestamp metadata in this version. Polling cannot detect changes that happen
and are reverted entirely between checks.

References: [OCI tag listing](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-tags),
[Docker Hub tag schema](https://docs.docker.com/reference/api/hub/latest/schemas/tag/).

### Public registries and quotas

This version deliberately uses anonymous access, including anonymous bearer-token
exchange where required. It does not load Docker credentials, cloud SDKs, or
credential helpers. A public image must also permit anonymous **tag listing**;
being able to pull a known tag alone is insufficient. Cloud registry IAM policies
can prevent listing even for publicly pullable images.

Remote manifests are checked with HEAD where supported, falling back to GET.
Requests are bounded by timeouts; the registry library retries selected transient
failures, and each selected tag is retried twice within a repository check.
Rate-limit and authentication errors are shown in the report and retried
on the next polling cycle. Registry request quotas still apply; filters reduce
manifest calls, but listing must discover all tags. `scope: pending` reduces them
further by trusting the state file for versions that are already running.

## Local development

Requires Go 1.25 or newer (the container builds with Go 1.27).

```sh
cp config.example.yaml config.yaml
# Set state_file to ./data/state.json for local use.
go run ./cmd/tagwatch -config config.yaml
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o tagwatch ./cmd/tagwatch
```

`-once` performs one check and saves state without starting HTTP; it exits nonzero
for repository or state-write errors. Docker inspection problems are warnings
in the normal service report and do not change the one-shot exit status.
The usual service mode starts HTTP immediately while the initial poll runs.
`GET /healthz` checks HTTP liveness, not registry freshness. Refresh the HTML page
manually to see the latest cached results. SIGINT/SIGTERM cancels polling and
gracefully shuts down HTTP.

Tests use local HTTP/Unix-socket fakes; no registry credentials, Docker daemon,
or internet connection are required after Go dependencies have been downloaded.
