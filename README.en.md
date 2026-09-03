<p align="center"><img src="brand/logo.svg" width="130" alt="m-ui"></p>
<h1 align="center">m-ui</h1>
<p align="center">Multi-server proxy panel with embedded <a href="https://github.com/SagerNet/sing-box">sing-box</a> · single binary · one master, any number of nodes · all day-to-day ops inside the panel</p>
<p align="center"><a href="README.md">中文</a> · <b>English</b></p>

---

## What it is

m-ui is a self-hosted proxy panel: one binary, one database file, sing-box embedded. Lines, users, subscriptions, certificates, backups, multiple servers and routine maintenance are all managed from the web UI.

What makes it different from the usual panels:

- **Line model**: a *line* is "inbound protocol + port → upstream". No separate inbound / outbound / routing tables to keep in sync.
- **Config changes don't drop users**: every save is dry-run through sing-box first, so a broken config never reaches the database. User and upstream changes are hot-swapped; only line changes restart the data plane.
- **One master, many nodes**: the master pushes lines / upstreams / users to any number of node servers, collects their traffic and enforces quotas centrally. Subscriptions list every line once per server and clients pick the lowest-latency entry automatically.
- **Ops built in**: Let's Encrypt, Cloudflare WARP, kernel tuning, backup / restore, a migration wizard, Telegram alerts, two-factor login and an external API.

Good for personal use, small teams, or anyone who needs one place to manage users across several servers.

## Features

| Area | What you get |
|---|---|
| Lines | Hysteria2, AnyTLS, TUIC, Trojan, VLESS (Reality / Vision), VMess, Shadowsocks (incl. 2022), SOCKS, HTTP, Mixed; WS / gRPC / HTTPUpgrade / HTTP transports; Hysteria2 port hopping; per-server deployment |
| Upstreams | VLESS / VMess / Trojan / TUIC / Hysteria2 / Shadowsocks / SOCKS exits, import by pasting a share link; latency test, periodic health checks, failure / recovery alerts; one-click WARP |
| Users | Quota, expiry, periodic reset, concurrent device limit (by source IP, merged across servers), up / down speed limits; auto-disable and kick on overuse or expiry; bulk create, bulk actions, CSV export |
| Plans | Templates for quota / duration / devices / speed / lines; apply on create, renew or extend |
| Subscriptions | Universal link and Clash formats; landing page in the browser (usage, expiry, one-tap import, QR); external nodes / external subscriptions merged in; `insecure` added automatically for self-signed certs |
| Multi-server | Master pushes config, collects traffic, enforces quotas; per-server traffic ratio; node offline / recovery alerts |
| Certificates | Let's Encrypt via HTTP-01 or Cloudflare DNS-01, auto-renewal, hot reload without restart; one-click self-signed cert when you have no domain |
| Backup | One zip with a consistent database snapshot and certificates; upload to restore a whole server; scheduled backups, rotation, push to Telegram; migration wizard |
| Ops | WARP install / enable / disable / uninstall with exit-IP verification; swap, sysctl + BBR, file limits, NTP |
| Alerts | Telegram: logins, user expiring / over quota / disabled, upstream failures, data plane down, daily report |
| Observability | Traffic charts (stacked bars, 1h / 6h / 24h / 7d / 30d), top users, online users and IPs, recent inbound connections for troubleshooting, audit log, subscription access log |
| Security | bcrypt passwords, CSRF protection, failed-login alerts, two-factor authentication (TOTP), external API tokens |
| Integration | External API for creating users, applying plans, enabling / disabling, renewing and fetching subscription URLs, see [docs/API.md](docs/API.md) |

## Installation

### Requirements

- Linux amd64 or arm64 with systemd (Debian 11+ / Ubuntu 20.04+ recommended)
- root
- Open ports: panel `2053/tcp`, subscription `2056/tcp`, plus the ports of the lines you create (TCP or UDP depending on protocol). HTTP-01 certificate issuance also needs `80/tcp` reachable

### One-line install (recommended)

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Maoyangui/m-ui/main/deploy/install.sh)
```

Not root? Use the piped form:

```bash
curl -fsSL https://raw.githubusercontent.com/Maoyangui/m-ui/main/deploy/install.sh | sudo bash
```

The script detects the architecture, downloads the latest release from GitHub, installs `/usr/local/bin/m-ui`, keeps data in `/etc/m-ui/` and registers `m-ui.service`. It prints the panel URL and default credentials when done.

Specific version or offline:

```bash
bash install.sh v0.2.0                              # pin a version
bash install.sh ./m-ui-linux-amd64.tar.gz           # local file (release tarball or bare binary)
bash install.sh https://.../m-ui-linux-arm64.tar.gz # any download URL
```

### First login

- Panel: `http://<server-ip>:2053/app/`
- Default account: `admin` / `admin`

A banner keeps reminding you until the default password is changed; do it on the **Admin** page right away. Port, path and credentials can also be changed from the SSH menu (`m-ui`).

The panel UI is available in Chinese and English (switch at the bottom of the sidebar). The SSH menu and install script are currently Chinese only.

### Upgrade

```bash
m-ui            # menu item 10 "update to latest"
```

or run the one-line installer again. Upgrades replace the binary and restart; database, certificates and backups are untouched.

### Uninstall

`m-ui` menu item 14, optionally removing the `/etc/m-ui` data directory.

## Quick start

The dashboard shows a **Quick start** checklist. In order:

1. **Domain** (optional): Settings → Panel → Domain. Point an A record at the server.
2. **Certificate**: on the Certificate page enter the domain and issue a Let's Encrypt cert; without a domain click "self-signed" with the server IP. Panel, subscription and data plane share the cert and it renews itself.
3. **Line**: Lines → Add, pick a protocol (Hysteria2 or AnyTLS recommended), a port and an upstream (direct by default).
4. **User**: Users → Add, tick the lines the user may use, or apply a plan.
5. **Hand out the subscription**: copy the subscription link from the user list, or let the user open the landing page in a browser to scan / import.

No domain is fine: self-signed cert + IP, and subscriptions carry "allow insecure" automatically. Issue a real certificate later and users only need to refresh once.

## Feature guide

### Lines

- One line = one entry port. Ports are identical on every server; the master renders and pushes the config to each node.
- The **upstream** decides where traffic exits: direct, WARP, or any relay you added.
- **Deploy to**: all servers by default, or only selected ones, e.g. a line that exists only on one node.
- Hysteria2 accepts a **port-hopping range** (e.g. `20000-30000`): the server forwards that UDP range to the line port with nftables / iptables and clients hop between ports, which sidesteps per-port UDP throttling by ISPs.
- Every save dry-runs the full sing-box config; a failing change is rejected instead of stored.

### Upstreams

- Edit visually or paste `vless://`, `hy2://`, `ss://` … share links.
- Latency test through the upstream, periodic checks; Telegram alert on repeated failure and again on recovery.
- Enabling WARP on the Ops page creates an upstream named `warp`; pick it on a line to exit through WARP.

### External nodes

Merge nodes from elsewhere into your subscriptions: a single share link, or a whole external subscription (provider / relay). The master fetches it on a schedule; once assigned to a user, those nodes appear after this site's nodes, optionally with a prefix.

### Users and plans

- Per user: quota, expiry, periodic reset (e.g. every 30 days), device limit, speed limit, remark, allowed lines and external nodes.
- Over quota or expired → disabled and kicked automatically; resetting traffic re-enables.
- Plans are templates: apply on create; *renew* applies again (usage reset, expiry extended); *extend* keeps usage and only extends expiry.
- Bulk: generate by prefix, select rows for enable / disable / extend / reset / delete, CSV export.
- User drawer: live devices (each IP shows the line and server it is using), 24h / 7d / 30d chart, subscription links and QR, kick.

### Subscriptions

- Universal: `https://<domain-or-ip>:2056/sub/<user>` for nextin, sing-box, Shadowrocket, Surge, Quantumult X, Loon, Karing and others.
- Clash: append `?format=clash` for mihomo / Clash Verge / Stash; each line becomes a latency-selected group of entries.
- sing-box: append `?format=json` for a complete sing-box client config (TUN, groups, DNS, basic routing) that SFA (Android), SFI (iOS) and the desktop build import as a remote profile; the landing page has a one-tap import button.
- Opening the link in a browser shows a landing page: usage, expiry, one-tap import for each client, QR codes, custom notice and support contact.
- Node addresses default to the **server IP** with the domain only as SNI, so a poisoned local DNS on the client cannot break connectivity; switch to domain in Settings if you prefer.
- With several servers each line appears once per server, suffixed with the server name; servers with a traffic ratio other than 1 get a tag such as `x2`.

### Multi-server

Any number of nodes. To attach one:

1. Install m-ui on the node, sign in, Settings → Role → "run as node".
2. Copy the API URL and token from Settings → Pairing on the node.
3. On the master, Servers → Add: name (e.g. "HK", used as the node-name suffix), domain, API URL, token; optionally skip certificate verification if the node uses a self-signed cert.
4. Within seconds the master pushes and shows "synced".

How it works: the master compares a snapshot every few seconds and pushes on change; in the same round it pulls the node's traffic ledger, online IPs, status and certificate expiry. Quotas are enforced only on the master; a disabled user reaches every node within about 5 seconds. A node unreachable for over a minute triggers one alert and one more on recovery; while offline it keeps serving users with its last config.

Each server, including the master, can have a **traffic ratio**: traffic through it counts toward users' usage multiplied by the ratio, e.g. 2× for an expensive route.

### Certificates

Three sources:
- **With a domain**: Let's Encrypt via HTTP-01 (port 80 reachable) or Cloudflare DNS-01 (API token, no port 80). DNS and port are pre-checked; auto-renewal reloads everything without a restart.
- **Without a domain**: self-signed certificate for the server IP. Used for line inbounds only by default; subscriptions automatically carry "allow insecure" so clients connect, while panel and subscription stay on HTTP (browsers and clients reject self-signed HTTPS).
- **Existing certificate**: point at a certificate and key already on the server (certbot, nginx, commercial…). Only paths are stored; they are checked for a matching, unexpired pair. Renew by overwriting the files.

Line inbounds always use the current certificate; **panel HTTPS and subscription HTTPS are separate checkboxes** (subscription applies immediately, panel needs an m-ui restart). Nodes keep their own certificates.

### Backup and migration

- Backup page: download a zip (consistent DB snapshot + certs); scheduled backups with rotation, optionally pushed to Telegram.
- Restore: upload the zip on the new server's Backup page and m-ui restarts fully restored, or `bash install.sh --restore backup.zip` at install time.
- Migration wizard: install and restore on the new server → check the panel → point DNS at the new IP (the page checks major resolvers) → retire the old server. Ports, credentials and subscription URLs stay the same, users notice nothing.

### Ops

- WARP: install / enable (local SOCKS5, MASQUE) / disable / uninstall, with exit-IP verification.
- System tuning: swap, sysctl + BBR, file-descriptor limits, NTP; parameters adjustable per machine. Tasks run one at a time with live output.

### Logs

Subscription access log, core log (data plane + panel) and audit log. Logging can be switched off on the core log tab to save resources, and every log can be cleared with one click.

### Admin

- Change username / password.
- **Two-factor authentication**: generate a secret and scan it, or paste an existing secret; enter one code to enable. Works with Google / Microsoft Authenticator, Authy, 1Password and more. Lost your phone? Run `m-ui` over SSH and pick "disable 2FA"; resetting the password also clears it.
- **External API**: flip the switch to get a token, then a shop / billing system / bot can create users, apply plans, enable / disable, kick and fetch subscription URLs. The page lists the endpoints with curl examples; full reference in [docs/API.md](docs/API.md).

### Settings

**Time zone** (default Asia/Shanghai; every time in the panel uses it), panel and subscription listen address / port / path / certificate, subscription display options, landing-page text, Telegram notification toggles, upstream check parameters, data-plane stats granularity. Panel and subscription path changes apply immediately; port, listen address and certificate path changes need a restart (button in the page header).

## Command line

```
m-ui                                         numeric menu (below)
m-ui run -db /etc/m-ui/m-ui.db               start (used by systemd)
m-ui info -db <db>                           print panel URL, path and account status
m-ui set -db <db> key=value ...              change settings, e.g. webPort=3053 nodeMode=true
m-ui passwd -db <db> [-password NEW]         reset the admin password (also disables 2FA)
m-ui backup -db <db> -out backup.zip         create a backup
m-ui restore -db <db> -from backup.zip       restore (service stopped)
m-ui import -from old-panel.db -to <db>      import an old panel database
m-ui selfsign -hosts domain,ip               generate a self-signed certificate
m-ui render -db <db>                         print and validate the sing-box config
m-ui version
```

Run `m-ui` without arguments for the menu (Chinese):

```
 1. panel URL / account            8. recent logs
 2. restart                        9. backup now
 3. stop                          10. update to latest
 4. start                         11. self-signed certificate
 5. reset admin password          12. switch master / node role
 6. change panel port / path      13. disable two-factor auth
 7. reset all settings            14. uninstall
```

## Migrating from an old panel

Full migration (lines, upstreams, users and settings):

```bash
bash install.sh --import /path/to/old-panel.db
```

Inbound ports, user credentials, panel path and subscription URLs are preserved, so clients need no changes; the import report lists what was checked.

Users only (lines already rebuilt on m-ui, you just want the old users' names, usage, quota and expiry): Users page → **Import from old panel**, or

```bash
m-ui import -from /path/to/old-panel.db -to /etc/m-ui/m-ui.db -users-only && systemctl restart m-ui
```

Existing names only get usage / quota / expiry / enabled updated; new users are created with their old credentials and, by default, every existing line.

## FAQ

**Clients can't connect but the subscription refreshes fine.** Usually the client's local DNS resolves the domain wrongly. m-ui writes the server IP into subscriptions by default (domain only as SNI); ask users to refresh once. "Recent inbound connections" on the dashboard shows whether the user's IP reaches the server at all.

**Can I run without a domain?** Yes. Generate a self-signed cert with the IP; subscriptions carry "allow insecure". Issue a real cert later and users just refresh.

**Changed the port / listen address and nothing happened?** Those need a restart (button in the Settings header, or menu item 2). Certificate renewal does not.

**WARP fails to enable?** The task output on the Ops page shows why; usually port 40000 is taken. WARP is only a local SOCKS5 upstream; pick upstream `warp` on a line to use it.

**A node shows offline.** The Servers page shows the reason (bad token / timeout / certificate error). Tick "skip certificate verification" on the master when the node uses a self-signed cert.

**Forgot the password or lost the 2FA phone.** Run `m-ui` over SSH: item 5 resets the password (and clears 2FA), item 13 only disables 2FA.

**Where is the data?** `/etc/m-ui/m-ui.db` (database), `/etc/m-ui/cert/` (certificates), `/etc/m-ui/backups/` (scheduled backups). Backup zips contain user credentials and private keys, keep them safe.

## Development

Go 1.22+. Reality needs the `with_utls` build tag, so every build and test must carry it:

```bash
make build          # host binary
make linux          # CGO_ENABLED=0 GOOS=linux static binary
make test           # go test -tags with_utls ./...
```

The frontend is zero-build ES modules in `web/assets/`; edit and refresh. Embedded assets get a content-hash ETag so browsers pick up new files after an upgrade. Architecture notes: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (Chinese).

```
core/      embedded sing-box lifecycle, traffic stats, connection tracking, limiter
render/    line → sing-box config rendering and validation
sub/       subscription generation (link / clash), landing page, subscription server
hub/       multi-server sync (snapshot push, traffic collection, online aggregation)
jobs/      stats, quota enforcement, cleanup jobs
monitor/   upstream checks, watchdog, user warnings, daily report
acme/      Let's Encrypt (HTTP-01 / Cloudflare DNS-01)
backup/    backup and restore
ops/       WARP and system tuning scripts
hop/       Hysteria2 port hopping (nftables / iptables)
ext/       external nodes / subscriptions fetching and parsing
totp/      two-factor authentication (RFC 6238)
web/       panel HTTP API and frontend
importer/  old panel database import
brand/     logo and contact details
```

Releases: pushing a `v*` tag makes GitHub Actions run the tests, build amd64 / arm64 and publish a release.

## Feedback and support

The "Feedback / Support" button in the panel's top-right corner has the author's Telegram and a donation address; issues and pull requests are welcome too.

## License

GPL-3.0. Embeds [sing-box](https://github.com/SagerNet/sing-box); third-party notices in [NOTICE](NOTICE).
