# Dashboard Startpage

A self-hosted browser startpage for homelab monitoring. Two containers run on
IncusOS:

- **Homepage** ([gethomepage.dev](https://gethomepage.dev)): Frontend startpage
  with built-in widgets for Nextcloud, Home Assistant, and weather. Uses
  `customapi` widgets for NixOS and Incus data from the Go backend.
- **dashboard-api**: A statically-compiled Go binary in a minimal container
  (busybox + CA certificates) that polls NixOS machines via Prometheus Node
  Exporter and scrapes the Incus metrics endpoint. Serves a JSON API for
  Homepage to consume.

No database. No historical data. Current state only. All communication over
Tailscale.

## Prerequisites

- NixOS host (for development and agent deployment)
- Incus client >= 6.1 (for OCI remote support). The dev shell (`nix develop`)
  provides a current client. If your system has an older version (e.g., 6.0.x
  LTS), make sure to run Incus commands from inside the dev shell.
- Incus on the target host (IncusOS)
- Tailscale network connecting all machines
- `agenix` for secrets management in your NixOS configurations
- The IncusOS host configured as an Incus remote on your local machine
  (see `incus remote add`)

## Initial Setup (Step by Step)

Follow these steps in order when setting up the project from scratch.

### 1. (Optional) Create a heartbeat check

Create a check at <https://healthchecks.io> (or a compatible service) with
a period equal to the poll interval. Note its ping URL -- it goes into
`[heartbeat] url` in `config.toml`. See [Heartbeat](#heartbeat).

### 2. Switch to the IncusOS remote and create storage volumes

IncusOS has an immutable root filesystem -- you cannot SSH in or write files
to the host directly. All interaction happens via the Incus API. Switch your
default remote to the IncusOS host so that all subsequent `incus` commands
target it automatically:

```bash
incus remote switch incus-host
```

Create custom storage volumes to hold secrets, config, and Homepage config.
These volumes persist across container rebuilds.

```bash
incus storage volume create local dashboard-secrets
incus storage volume create local dashboard-config
incus storage volume create local homepage-config
```

### 3. Initialize Containers and Attach Volumes

Build the backend image, initialize the containers, and attach the storage volumes.
This sets up the filesystems so we can push configuration into them.

```bash
# Build the Go API image (produces an Incus-native tarball)
nix build .#dashboard-api-image

# Import the image into the remote Incus server
incus image import ./result --alias dashboard-api-img

# Initialize Dashboard API container and attach volumes
incus init dashboard-api-img dashboard-api
incus storage volume attach local dashboard-secrets dashboard-api /secrets
incus storage volume attach local dashboard-config dashboard-api /config

# Add the GitHub Container Registry OCI remote (one-time setup)
incus remote add ghcr https://ghcr.io --protocol=oci

# Initialize Homepage container and attach volume
incus init ghcr:gethomepage/homepage:latest homepage
incus storage volume attach local homepage-config homepage /app/config

# Allow the IncusOS Tailscale hostname to pass Homepage's host validation
incus config set homepage environment.HOMEPAGE_ALLOWED_HOSTS=incusos.tail2c589.ts.net:3000
```

### 4. Generate and Push Secrets

Generate a metrics-only TLS client certificate for the Incus metrics endpoint.

```bash
# Generate the certificate locally
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:secp384r1 \
  -sha384 -keyout secrets/metrics.key -out secrets/metrics.crt -nodes -days 3650 \
  -subj "/CN=dashboard-metrics"

# Trust the certificate in Incus
incus config trust add-certificate secrets/metrics.crt --type=metrics

# Push to the container volume
incus file push secrets/metrics.crt dashboard-api/secrets/metrics.crt
incus file push secrets/metrics.key dashboard-api/secrets/metrics.key
```

Generate the node-exporter Basic Auth password. The plaintext goes to the
container, the bcrypt hash goes to your NixOS configurations.

```bash
# Generate a random password
PASS=$(openssl rand -base64 24)
echo -n "$PASS" > secrets/node-exporter-pass

# Push the plaintext password to the container
incus file push secrets/node-exporter-pass dashboard-api/secrets/node-exporter-pass

# Generate bcrypt hash for agenix (needs htpasswd or similar)
htpasswd -nbBC 10 "" "$PASS" | tr -d ':\n'
```

Store the bcrypt hash via `agenix` in your NixOS configurations. Every
monitored machine needs access to this secret.

### 5. Add the dashboard flake input to your NixOS configs

In your NixOS flake (the one managing your machines), add this repository as
an input:

```nix
inputs.dashboard.url = "github:ole/dashboard"; # or a local path
```

Then import the agent module on each machine you want to monitor:

```nix
# hosts/server-01/default.nix
{
  imports = [ inputs.dashboard.nixosModules.agent ];

  services.dashboard-agent = {
    enable = true;
    basicAuthPasswordFile = config.age.secrets."node-exporter-pass".path;
    customChecks = {
      smart = true;              # if the machine has physical disks
      borgJobs = [ "default" ];  # if the machine runs NixOS borgmatic/borgbackup
      pikaBackupUsers = [ "ole" ]; # if a desktop user runs Pika Backup
    };
  };
}
```

This enables `prometheus-node-exporter` with Basic Auth, the `systemd`
collector, and textfile scripts for SMART, Borg, NixOS generation, and reboot
detection.

Apply on each machine:

```bash
nixos-rebuild switch
```

### 6. Create config.toml

```bash
cp config.example.toml config.toml
```

Fill in:
- Your actual Tailscale hostnames (e.g., `server-01.tailnet-name.ts.net`)
- The Incus host URL
- The heartbeat ping URL from step 1, if you created one
- Paths to the TLS cert/key from step 3 (inside the container these are
  `/secrets/metrics.crt` and `/secrets/metrics.key`)
- Path to the node-exporter password file (`/secrets/node-exporter-pass`)
- Set `critical = true/false` for each machine (reported in the status JSON)
- **Important**: Node Exporter URLs must use `http://`, not `https://`.
  Node Exporter serves plain HTTP by default.

See `config.example.toml` for full documentation of all options.

Push `config.toml` into the dashboard API container:

```bash
incus file push config.toml dashboard-api/config/config.toml
```

### 7. Update Homepage config templates

Edit `homepage/services.yaml`:
- Replace placeholder hostnames with your real machine names
- Replace `cloud.example.com` with your real Nextcloud URL
- Replace `homeassistant.tailnet-name.ts.net` with your real Home Assistant URL

Edit `homepage/widgets.yaml`:
- Set your latitude/longitude and timezone

You will also need:
- A **Nextcloud Serverinfo API token**. The **Monitoring** app (serverinfo)
  must be enabled in the Nextcloud admin panel under **Apps**. The token
  cannot be generated from the web UI. On your Nextcloud server, run:
  ```bash
  openssl rand -hex 32
  ```
  Then set the token via `occ`:
  ```bash
  occ config:app:set serverinfo token --value <generated_token_value>
  ```
- A **Home Assistant long-lived access token** (generate one from your HA
  profile page at `https://<your-ha-url>/profile`). No username is needed;
  the token is the only authentication for the widget.

Push the homepage config files directly into the homepage container:

```bash
incus file push homepage/services.yaml homepage/app/config/services.yaml
incus file push homepage/settings.yaml homepage/app/config/settings.yaml
incus file push homepage/widgets.yaml homepage/app/config/widgets.yaml
incus file push homepage/bookmarks.yaml homepage/app/config/bookmarks.yaml
incus file push homepage/docker.yaml homepage/app/config/docker.yaml
```

### 8. Start Containers and Network Forwarding

The containers sit on an Incus bridge network. To reach them from Tailscale,
add proxy devices that forward ports from the IncusOS host into the
containers. Finally, start the containers.

```bash
# Homepage (port 3000)
incus config device add homepage proxy-http proxy \
  listen=tcp:0.0.0.0:3000 connect=tcp:127.0.0.1:3000

# Dashboard API (port 8080) — optional, only needed for direct access;
# Homepage reaches it via the Incus bridge (http://dashboard-api.incus:8080)
incus config device add dashboard-api proxy-http proxy \
  listen=tcp:0.0.0.0:8080 connect=tcp:127.0.0.1:8080

incus start dashboard-api
incus start homepage
```

You can now access Homepage at `http://incusos.tail2c589.ts.net:3000` from
any machine on your Tailnet.

Verify the API is running:

```bash
curl http://dashboard-api.incus:8080/healthz
```

## Development

Enter the dev shell (provides Go, gopls, and other tools):

```bash
nix develop
```

Build the Go binary locally:

```bash
go build -o dashboard-api ./cmd/dashboard-api
```

Run with a config file:

```bash
./dashboard-api -config config.toml
```

## Build & Deploy

### Build the image

```bash
nix build .#dashboard-api-image
```

### Deploy the Go API container

For day-to-day deploys, use the deploy script:

```bash
./deploy.sh
```

The Go binary boots in milliseconds. Homepage handles the brief API outage
gracefully.

### Update Homepage

```bash
incus stop homepage
incus rebuild ghcr:gethomepage/homepage:latest homepage
incus start homepage
```

The config is attached as a storage volume, so it survives rebuilds.

### Update NixOS agents

On each monitored machine:

```bash
nixos-rebuild switch
```

## Secrets

Secrets are stored in the `dashboard-secrets` Incus storage volume and
attached to the dashboard-api container at `/secrets/`. The node-exporter
bcrypt hash is managed via `agenix` in your NixOS configurations.

| Secret                  | Location                       | Purpose                                     |
|-------------------------|--------------------------------|---------------------------------------------|
| `metrics.crt`           | `dashboard-secrets` volume     | TLS client cert for Incus metrics endpoint  |
| `metrics.key`           | `dashboard-secrets` volume     | TLS client key for Incus metrics endpoint   |
| `node-exporter-pass`    | `dashboard-secrets` volume     | Basic Auth password for node-exporter       |

The `config.toml` file is stored in the `dashboard-config` Incus storage
volume. Do not commit `config.toml` to version control (it is in
`.gitignore`).

## API Endpoints

| Method | Path                       | Description                           |
|--------|----------------------------|---------------------------------------|
| GET    | `/api/v1/status`           | All NixOS machines + Incus metrics    |
| GET    | `/api/v1/status/:hostname` | Single machine metrics                |
| GET    | `/healthz`                 | Health check (returns 200 if running) |

## Heartbeat

The collector does not send alerts. It reports state on `/api/v1/status`;
deciding what is worth a notification is the job of whatever reads that.

What it does send is a heartbeat: with `[heartbeat] url` set, it requests
that URL after every poll cycle in which at least one target answered. Point
it at a dead-man's-switch service such as healthchecks.io, which then alerts
when the pings stop -- that is, when the collector, its host or its network
is down. A cycle that reaches no target at all sends no ping.

The ping URL is a credential: anyone who has it can keep the check green.
It is never written to the log.

Each machine's `critical` flag from `config.toml` is passed through in the
status JSON so consumers can tell always-on servers from laptops.

## Architecture

See `.opencode/plans/dashboard-plan.md` for the full implementation plan,
including architecture diagrams, data flow, design decisions, and detailed
metric tables.
