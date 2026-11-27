# AtAvix

AtAvix is a self-hosted control panel for VLESS subscriptions. The English-language dashboard uses a black and violet theme and gives each user a separate quota, connection UUID, and private subscription link.

## What you get

- Add, search, edit, pause, or delete users. Set each user's quota in GB and choose a custom remark.
- Share one subscription link per user. Each connection profile combines your remark with the operator name. The subscription also includes a display-only balance such as `2.6 of 20 GB`, recalculated on every refresh. All profiles share the same quota. You can publish additional routes with operator names in their remarks.
- Use one hostname for everything or separate the admin panel and client connections, for example `https://panel.example.com/admin/` and `https://sub.example.com/<subscription-uuid>`.
- Choose SQLite or a managed PostgreSQL container during setup.
- Optionally record domain names for individual users. Tracking starts off and can be disabled or cleared independently for each account.

## Requirements

- A Linux server on x86-64 or ARM64 with root or sudo access. Automatic Docker installation uses the official Docker repository on Debian and Ubuntu. Other Linux distributions can run AtAvix with a working Docker Engine, Compose plugin with `up --wait` support, and Bash installed.
- Git, an outbound internet connection, and enough space to build the image. Allow about 2 GB of RAM and 5 GB of free disk for the first build. The runtime footprint is smaller.
- For automatic public HTTPS, a DNS hostname and inbound TCP ports 80 and 443. Allow UDP 443 if you want HTTP/3. Keep your SSH port accessible when changing firewall rules.

## Install

On Debian or Ubuntu, install Git if needed.

```bash
sudo apt-get update
sudo apt-get install -y git
```

Clone the project on the server and run the wizard.

```bash
git clone https://github.com/Atila-Pasha/AtAvix.git
cd AtAvix
sudo ./main
```

The installer detects Docker and Compose and offers to install them if either is missing on Debian or Ubuntu. It builds the panel, downloads the pinned Xray release, and verifies its SHA-256 checksum.

1. Select **SQLite** for a single-file database or **PostgreSQL** for a separate database container. The wizard creates the PostgreSQL password automatically and keeps its port private.
2. Select an access mode from the options below. In either public mode you can enable direct TCP + REALITY and a separate XHTTP + REALITY listener.
3. Enter an administrator username and a password of 12–72 bytes. The installer writes configuration to `.env` and stores the password in the restricted `.atavix/admin-password` file. Back up both securely.
4. Open the printed panel URL and sign in. The first build can take several minutes.

### Option 1 · Automatic public HTTPS

1. Create an `A` record for each hostname at your DNS provider. Point both records to the server's public IPv4 address. Add `AAAA` records only if the host accepts inbound connections over IPv6. For the initial setup, use DNS-only records if your provider has a proxy switch.
2. Allow inbound TCP ports 80 and 443 in the server firewall and any cloud security group or router. If a web server is already using those ports, use option 2.
3. Enter the admin hostname, such as `panel.example.com`. Accept the same hostname for subscriptions or enter another, such as `sub.atilapasha.ir`, if you control its DNS. Enter an email address for certificate registration.
4. Wait for Caddy to issue a certificate, then open `https://panel.example.com/admin/`. Separate subscription hosts serve subscriptions and WebSocket endpoints.

Automatic HTTPS renews certificates using the Caddy data volume. For certificate failures, check DNS resolution, the firewall, and `sudo ./main logs`. [Caddy automatic HTTPS documentation](https://caddyserver.com/docs/automatic-https)

### Option 2 · Existing HTTPS reverse proxy

Enter the localhost port for the backend and the public HTTPS origins. Point your reverse proxy to `127.0.0.1:<port>`. Preserve the incoming `Host` and allow WebSocket upgrades on `/connect` and `/fallback`. Keep subscription responses uncached and exclude their URL paths from access logs.

This example belongs inside the matching Nginx TLS server block. Configure a valid certificate for each public hostname.

```nginx
location / {
    access_log off;
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Atavix-Client-IP $remote_addr;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 150s;
    proxy_buffering off;
}
```

Add the upgrade map once to Nginx's `http` block.

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
```

Once your reverse proxy overwrites `X-Atavix-Client-IP` as shown, you can add `ATAVIX_TRUST_PROXY=true` to `.env` and run `sudo ./main`. This makes sign-in rate limits apply to individual client IPs.

### Option 3 · Local access or SSH tunnel

This option binds the panel to loopback. For a remote server, run this on your workstation and leave the SSH session open.

```bash
ssh -N -L 8080:127.0.0.1:8080 user@your-server
```

Open `http://127.0.0.1:8080/admin/` on the workstation. Use the chosen port in both places if you changed it. Subscriptions in local mode are usable by clients with access to that loopback endpoint, such as a client on your workstation using the SSH tunnel. Run `sudo ./main configure` when you have a public HTTPS endpoint.

### Direct TCP and XHTTP with REALITY

The public installer offers a primary VLESS profile served directly over TCP + REALITY. Use a DNS-only hostname or an IP that reaches the server directly, and allow the chosen TCP port through the server and provider firewalls. The default is 8443 when the panel's automatic HTTPS listener uses 443. Use 443 for REALITY when it is free on the server. The wizard also offers XHTTP + REALITY on a second TCP port, 8444 by default. Allow that port through the server and provider firewalls. The HTTPS subscription and WebSocket fallback use the configured web endpoint.

For the REALITY TLS cover hostname, use a TLS 1.3 site that you control with HTTP/2 enabled, ideally on the same host or near the server. The automatic Caddy setup can use its own panel or subscription hostname. In that mode the direct profile becomes usable after Caddy has obtained the certificate. A TLS cover hostname is separate from the public address that clients connect to. AtAvix generates and stores the X25519 identity and short ID in the database, so normal restarts preserve existing profiles.

Direct TCP avoids WebSocket framing and a CDN hop. Field reports from Iran show that reachability and latency vary by network, server IP, and route. Direct TCP is a good first latency test. XHTTP provides a second direct option using HTTP request streams over REALITY. WebSocket + TLS uses the HTTPS endpoint. Compare all three profiles from the actual ISP at the times your users connect. In your client, repeat the connection delay test and compare typical latency, slow outliers, and failures. [Iran network field measurements](https://github.com/saroo98/iran-network-field-guide) · [Iran connection quality measurements](https://radar.cloudflare.com/quality/ir) · [Xray REALITY documentation](https://xtls.github.io/en/config/transports/reality.html) · [Xray XHTTP documentation](https://xtls.github.io/en/config/transports/splithttp.html)

Existing installations can update with `git pull --ff-only` and `sudo ./main update`. Then run `sudo ./main configure` to enable REALITY and refresh subscriptions in the clients.

## Create and share a subscription

1. Open **Users → Add user**.
2. Enter a unique username, a connection remark, and the traffic quota. Enter `10` for 10 GB. The panel generates the connection UUID and the separate subscription UUID automatically.
3. Open the user's details and select **Copy link**. Share the link privately. Anyone with the link can download that user's connection credentials and consume the quota.
4. Open the link in a browser to see the suggested client for your device. On Android, **Import all profiles** opens v2rayNG with the full list of profiles. On v2rayN and Hiddify, use **Copy all profiles** and import them from the clipboard. Hiddify also offers **Add as a subscription**.
5. To receive updates, add the subscription URL to your client and refresh it after use. Direct imports are snapshots. The balance row shows the remaining amount followed by the total, for example `2.6 of 20 GB`. The row uses a reserved DNS name and placeholder UUID so it cannot connect.

The subscription page includes the original client icons for identification. [v2rayNG](https://github.com/2dust/v2rayNG) · [v2rayN](https://github.com/2dust/v2rayN) · [Hiddify](https://github.com/hiddify/hiddify-app)

To stop connections temporarily, disable the account in its details. Increasing its quota allows an exhausted account to resume if enabled. Deleting an account disconnects it and revokes its link and profiles. Moving the public hostname changes the origin of existing subscription URLs, so send the new links to users.

### Operator profiles

Open **Settings → Operator profiles**. Test a route from a connection on the intended ISP before assigning that operator's name. Field observations can guide which routes to test and results can change with the server IP and network conditions.

1. Enter the operator name, for example `Irancell` or `MCI`.
2. Select **TCP + REALITY**, **XHTTP + REALITY**, or **WebSocket**. Leave **Public endpoint** empty to use the installation's endpoint for that transport.
3. For an alternate path, enter a REALITY `hostname:port` or a WebSocket HTTPS origin. Alternate REALITY addresses must forward TCP to the same listener. Alternate WebSocket origins need valid certificates and must reverse-proxy `/connect` to this AtAvix instance.
4. Select **Save profiles**. Existing subscription links pick up the changes at refresh. With remark `Home`, the client displays `Home (Irancell)` for that route. The balance row retains the remaining and total amounts.

Routes are emitted in the order shown in Settings. Up to 12 operator profiles can be published, plus the balance profile. Each operator route needs a distinct combination of transport and endpoint. Operator routes appear when their transport is enabled. With both REALITY listeners enabled, the default suggestions are MCI (TCP + REALITY), Irancell (WebSocket + TLS), and Rightel (XHTTP + REALITY). WebSocket-only installations publish MCI and Irancell over separate WebSocket paths. Treat the labels as starting points and test them from the intended networks.

### How traffic and domain activity work

- A GB is 1,000,000,000 bytes. AtAvix counts uploads and downloads across all profiles, including VLESS headers and tunneled framing. Once the shared allowance is exhausted, open tunnels close and new connections are rejected. The dashboard and subscriptions read the current in-memory counter.
- Usage is checkpointed approximately once per second and saved on graceful shutdown. An abrupt crash can lose traffic recorded after the last successful checkpoint.
- Domain activity is opt-in per account. Enable it with the user's knowledge and permission. AtAvix records only hostnames explicitly sent in direct VLESS requests, with the most recent observation time. IP-only requests and multiplexed connection destinations are absent, and no page paths or message contents are stored. Up to 100 recent domain names are retained for seven days and clearing the toggle removes current records.
- WebSocket early data is disabled. Keep the imported WebSocket path and TLS settings as provided. Xray blocks private and reserved destinations and common outbound mail ports.

## Operate the service

Run commands from the checkout.

| Task | Command |
| --- | --- |
| Start, or resume after a failed setup | `sudo ./main` |
| Show container status | `sudo ./main status` |
| Follow recent logs | `sudo ./main logs` |
| Stop services and retain data | `sudo ./main stop` |
| Change the access mode or public hostnames | `sudo ./main configure` |
| Change the administrator password | `sudo ./main password` |
| Update from GitHub | `git pull --ff-only`, followed by `sudo ./main update` |

Use one AtAvix panel process per database. Configuration changes preserve the chosen database. The wizard does not migrate data between SQLite and PostgreSQL.

### Back up and restore

Backups contain connection credentials, the REALITY private key when enabled, traffic records, and any recorded domain activity. Store them privately.

**SQLite**

Stop the service for a consistent file backup. With the default Compose project name, the data volume is `atavix_atavix-data`.

```bash
umask 077
sudo ./main stop
sudo docker run --rm -v atavix_atavix-data:/source:ro alpine:3.23 \
  tar -C /source -czf - . > atavix-sqlite.tar.gz
sudo ./main
```

To restore, first stop the target installation and back up its current volume. Restore to an empty target volume or clear the target database files after making a backup.

```bash
sudo ./main stop
sudo docker run --rm -i -v atavix_atavix-data:/target alpine:3.23 \
  tar -C /target -xzf - < atavix-sqlite.tar.gz
sudo ./main
```

**PostgreSQL**

With PostgreSQL running, use the standard container `pg_dump` command. Add the Caddy override to the Compose command only if you need to manage the Caddy service.

```bash
umask 077
sudo docker compose -f compose.yaml -f compose.postgres.yaml exec -T postgres \
  pg_dump -U atavix -d atavix -Fc > atavix-postgres.dump
```

Restore to an empty installation with the panel stopped and the database running.

```bash
sudo docker compose -f compose.yaml -f compose.postgres.yaml stop panel
sudo docker compose -f compose.yaml -f compose.postgres.yaml up -d --wait postgres
sudo docker compose -f compose.yaml -f compose.postgres.yaml exec -T postgres \
  pg_restore --exit-on-error -U atavix -d atavix < atavix-postgres.dump
sudo ./main
```

Preserve `.env` and `.atavix/admin-password` along with the database backup. The password file is owned by container UID 10001 and should stay readable by that UID. Preserve the Caddy volumes when moving an installation if you want to retain issued certificates.

### Troubleshooting

| Symptom | Check |
| --- | --- |
| Docker installation reports conflicting packages | Check Docker's official install guide for your distribution. |
| Certificate has not issued | Check the hostnames point to the server, ports 80 and 443 are reachable, and no other service has bound those ports. |
| A client imports both profiles but cannot connect | Check the user is active with quota left, the URI points to the public proxy host, and the reverse proxy supports WebSockets. |
| The balance profile still shows the previous volume | Update the subscription in the client. The name is generated when the link is fetched. |
| The user has no domain activity | Check that tracking is enabled and the client forwards domain names rather than IP addresses or multiplexed requests. Give the checkpoint a moment to record the latest connection. |

[Docker Engine for Debian](https://docs.docker.com/engine/install/debian/) · [Docker Engine for Ubuntu](https://docs.docker.com/engine/install/ubuntu/) · [Docker Compose plugin](https://docs.docker.com/compose/install/linux/) · [Xray VLESS share-link format](https://github.com/XTLS/Xray-core/discussions/716)

## Run from source

Use Go 1.26.11 or later, a C compiler for the SQLite driver, and Xray 26.11.9 on Linux.

```bash
mkdir -p .atavix
chmod 700 .atavix
read -r -s -p 'Local admin password (12–72 bytes): ' admin_password; printf '\n'
(umask 077; printf '%s' "$admin_password" > .atavix/admin-password)
unset admin_password
go run .
```

This uses `http://127.0.0.1:8080/admin/`, the `admin` username, and `atavix.sqlite` by default. Set `XRAY_BIN` to the path to the Xray binary if it is outside your `PATH`.

| Variable | Purpose |
| --- | --- |
| `ADMIN_USERNAME`, `ADMIN_PASSWORD_FILE` | Admin username and path to the password file |
| `HTTP_ADDR` | HTTP bind address |
| `PANEL_URL` | Public admin origin |
| `SUBSCRIPTION_URL` | Origin used for subscription links. Defaults to `PANEL_URL` |
| `PROXY_URL` | Origin embedded in VLESS profiles. Defaults to `SUBSCRIPTION_URL` |
| `DATABASE_URL` | SQLite path or PostgreSQL connection URL |
| `XRAY_BIN` | Xray executable |
| `REALITY_PUBLIC`, `REALITY_SERVER_NAME` | Enable REALITY with a public `host:port` and cover TLS hostname |
| `REALITY_LISTEN`, `REALITY_TARGET` | Listener IP and port (default `0.0.0.0:8443`) and cover address (default `REALITY_SERVER_NAME:443`) |
| `XHTTP_PUBLIC`, `XHTTP_LISTEN` | Optional second REALITY endpoint for XHTTP, with listen address defaulting to `0.0.0.0:8444` |
| `TRUST_PROXY` | Enable only behind a trusted reverse proxy that overwrites `X-Atavix-Client-IP` |

Run `go test ./...` for the SQLite and subscription tests. Set `TEST_POSTGRES_URL` to run the accounting test against a disposable PostgreSQL database. Set `XRAY_TEST_BINARY` to run the WebSocket, TCP + REALITY, and XHTTP + REALITY connection tests against `example.com`.
