# Operate the service


### Build a package with your own options

A package overlay tells Poudriere to build a specific port for your VMs, even before a client asks for it. Poudrine offers only local builds made with those settings, including when FreeBSD has a newer version. Until the first matching build is ready, the package stays unavailable, even if Poudrine already cached the stock archive. After that, clients can keep using the matching build while an update compiles or if an update fails.

**Example — build mydumper with MariaDB 12.3**

1. Open **Management → Service settings → Poudriere builds → Package overlays** and choose **Add port**.
2. Enter `databases/mydumper` for **Port origin** and `mysql=12.3m` for **Default versions**. For a host running `mariadb123-server-12.3.3`, this selects the `mariadb123-client` library used by mydumper. The selector follows the 12.3 series across patch updates. [FreeBSD mydumper port](https://github.com/freebsd/freebsd-ports/blob/main/databases/mydumper/Makefile) · [Supported MySQL and MariaDB versions](https://github.com/freebsd/freebsd-ports/blob/main/Mk/Uses/mysql.mk)
3. Save settings. The worker builds the port in its next allowed build window. **Run build now** requests an earlier run within working hours. A build already in progress can finish while the repository starts enforcing the new overlay.

You can also add the overlay directly to your existing `/usr/local/etc/poudrine.json`.

```json
{
  "package_overlays": {
    "databases/mydumper": {
      "default_versions": { "mysql": "12.3m" }
    }
  }
}
```

On a VM with a locked local copy, keep the lock while the build runs. Refresh the catalog and check the dependency that clients will actually install.

```sh
pkg update -f -r Poudrine
pkg rquery -r Poudrine '%dn-%dv' mydumper
```

Once the output lists `mariadb123-client`, switch the VM to the Poudrine build. The forced install handles an existing package at the same version.

```sh
pkg unlock mydumper
pkg install -f -r Poudrine mydumper
```

Leave it unlocked to receive future builds through `pkg upgrade`.

For other ports, use `options_set` and `options_unset` arrays for named port options, and `default_versions` for dependency selectors supported by that port. For example, `"options_unset": ["DOCS"]` disables documentation on a port that offers the `DOCS` option. An empty object such as `"www/nginx": {}` requests a local build with the port’s normal settings.

Each overlay covers one port origin, including its flavors and subpackages. Changing its settings requires a matching build even at the same package version. Removing the entry restores normal repository selection for that origin.

### Builds and inventory

Poudrine checks the FreeBSD metadata every five minutes by default. When the mirror supports conditional HTTP requests, an unchanged signed catalog can be revalidated without downloading the archive again. Changed metadata still requires signature verification before publication. If a check fails, Poudrine retries within a minute.

**Build activity** shows overall progress and the next scheduled run or retry. **Requested ports** counts the entries on the build list. Each entry identifies a port by its path in the FreeBSD ports tree, such as `www/firefox`. FreeBSD calls that path the port’s *origin*. Poudriere adds any dependencies it needs to build to the package progress total.

**Compiling packages** shows a slot for each parallel builder. An available builder shows **Idle**. An occupied slot shows the package, phase, elapsed time, CPU and resident memory used by the builder jail. Elapsed time includes pauses. CPU usage of 100% represents one core. Installations with more than 128 builders can page through their slots.

**Phase** shows the build tool’s percentage when available. Otherwise, a percentage labeled **est.** compares active build time with the last successful build of the same package, flavor and ABI. Hover over **Elapsed** to see the estimated time left or how far the build has run past that estimate. Working-hours and service-restart pauses freeze the estimate. A pause in the previous build can make the estimate too high.

**Run build now** updates the jail and ports tree and starts an incremental bulk build. **Queue next build** reserves one follow-up while a build is active. With no tracked origins, the worker still updates the jail and ports tree. Between scheduled runs it checks for changes to the tracked origins every 30 seconds.

Open **Build history → Build run history** for the latest run’s package results. The selector lists each retained run’s start time and compiled package count. Use **←** for an older run and **→** for a newer one. Choose **All runs** to include empty runs or **With results** to see runs with built, failed or skipped packages.

Choose **All** to see the entire run, or select the **Succeeded**, **Failed**, or **Skipped** count to see those packages. The counts always cover the whole run. The circular arrow beside **Failed** or **Skipped** queues the packages with that outcome. Poudriere builds any prerequisites they need.

Select the document icon at the end of a package row to open its build log. For example, if `devel/abseil` fails during `fetch`, its log shows Poudriere’s fetch output and the skipped package entries identify what blocked them. **Load earlier output** reads back to the beginning. For a running package, use **Refresh latest output** to follow its progress.

The circular arrow in a package row retries that package with its affected dependency group. A failed package’s phase and reason appear beside its name. Expand **Skipped dependents** to see which packages it blocked. The worker starts a fresh incremental Poudriere run in the configured working hours, after any active build finishes. The new run records the retry result and the earlier run keeps its original result. Poudriere reuses healthy archives and builds missing packages locally. Diagnosis uses the retained files under `<basefs>/data/logs/bulk/<jail>-<ports>/<run>/`. The web service account needs read access there. Retrying requires the supervised service.

**Build history** counts packages by the UTC hour, day, week or month they finished. A run that builds 20 packages contributes 20 completions, and the chart shows failed package attempts separately. Click or tap a bar to see the packages and outcomes for that period. With a keyboard, move between bars using the arrow keys and select with Enter or Space. The list stays open as you move around the dashboard. Use **Show more packages** to page through busy periods. Expand **Recent package builds** to see the last 20 results across the chart, including finish times and links to logs. Poudrine recovers earlier results from retained Poudriere build logs. Fetched, skipped and already up-to-date packages contribute zero.

Enable the bell in Management to receive one notification for each newly built package whose version is higher than its counterpart in the verified FreeBSD catalog. Each notification shows the package and both versions. For example, a local `nginx` build at `1.28.0_2` qualifies when FreeBSD has `1.28.0_1`. Poudrine retries temporary delivery failures for up to seven days after the build finishes.

Under **Management → Service settings → Poudriere builds**, set **Working hours** to run builds when power or compute is cheaper. Enter daily ranges in the server’s local time using a 24-hour clock. For example, `00:00-10:00,11:00-17:00` allows both overnight and solar hours. A range can cross midnight, such as `22:00-06:00`. Leave the field empty for unrestricted builds. When a window closes, running compilers pause in memory. They resume when the next window opens. Keep the server and service running during the pause.

**Parallel package builders** controls how many packages Poudriere builds at once. **Make jobs per package** controls parallel compilation within each package, and **Make jobs limit per package** caps it. For example, two builders with four make jobs each can run up to eight make jobs at once. Ports that require serial builds still run serially.

**Repository clients** shows each IP, its unique package count and its last visit. Select **View packages** to see the names, requested versions and versions in the client’s latest installed-package report. A package that appears in both counts once, even when the versions differ. A client that has only requested catalog metadata can have zero packages.

During a bulk build, Poudrine checks finished archives every 15 seconds. For example, when Firefox finishes building with dav1d 1.5.5 and the repository still offers 1.5.4_1, Poudrine looks for the 1.5.5 package in the worker’s staging directory. Once both archives pass their publication checks, clients can run `pkg update` and install the new versions while other ports are still compiling.

Package inventory labels a locally built archive **Compiled**. A binary dependency the worker obtained from its package repository appears as **Fetched by Poudriere**. Poudrine trusts the worker’s native fetch records and requires the staged archive to match its download cache, the dependency’s identity and the target ABI. Poudrine signs the catalog with the checksum of the bytes it serves. If the newer archive is unavailable, a compatible published dependency can still satisfy Firefox’s declared package and shared-library requirements.

If a required library is missing, Package inventory names it and Poudrine checks again as providers become available. Updates that remove a previously provided library and packages with explicit `dep_formula` expressions wait for Poudriere’s final repository.

At the end of the run Poudrine switches to Poudriere’s committed repository. Budget temporary disk space for one extra copy of the packages published during a build. Poudrine keeps their download URLs for at least 24 hours after the handoff so clients can finish using an earlier catalog. A `local_repo` pointing to a different repository follows that repository’s own publication schedule.

**Package inventory** can be searched by port, package name or full package ID, such as `firefox-161.0,2`. **Build history only** means Poudriere recorded a successful build and the current catalog has no matching compiled archive. The row shows the publication blocker when known, such as an unreadable archive or a missing dependency. After a restart, Poudrine checks the archives still in Poudriere’s staging directory against the native build records. **Compiled** shows the size of an available local archive.

To see which Firefox version clients can install, refresh the signed repository on a client and query it directly.

```sh
pkg update -f -r Poudrine
pkg rquery -r Poudrine '%n-%v' firefox
```

Use **Compiled** for local builds, **Cached** for downloaded archives, or **Different from FreeBSD** for compiled archives whose version differs from FreeBSD’s. After a successful catalog check, Poudrine removes cached copies when a matching local build has the same or a newer version. A newer FreeBSD download stays cached.

Management shows the number and size of compiled archives and cached downloads. **Poudriere disk usage** covers its base directory and distfiles, including packages stored inside the base directory. The worker measures this at startup and every 15 minutes.

**Port version** comes from the latest Poudriere bulk run’s resolved ports list for the configured jail and tree. **Local (FreeBSD)** compares the compiled version with the version in the verified FreeBSD catalog. A dash means the value is unavailable. **Consumers** counts distinct client IPs that requested the package or included a catalog match in their latest report. Repeated downloads from one IP count once. Hover over **Last activity** for the exact date and time.

### Configuration, certificates and upgrades

**Management → Service settings** and manual edits both update `/usr/local/etc/poudrine.json`. An omitted field takes its default. Use lowercase JSON field names, duration strings such as `"15m"`, and numbers for cache size and job counts. Validate a candidate with `poudrine check-config -config /path/to/candidate.json`. Keep it beside the active file when testing relative paths. The supervised service applies working hours to the running build. Settings that require restarting the worker take effect between runs. Invalid edits leave the current service running.

| Settings | Defaults or use |
| --- | --- |
| `repo_listen`, `web_listen` | HTTP `[::]:8080`, HTTPS `[::]:8181`. Set these to your private interfaces or loopback. |
| `repo_url`, `web_url` | Optional advertised addresses. Set `repo_url` to an address resolvable by the clients. Include any port and reverse-proxy path prefix. |
| `web_tls`, `tls_cert`, `tls_key` | HTTPS enabled. Leave both file paths empty to generate a self-signed certificate in the state directory. |
| `state`, `service_user` | `/var/db/poudrine`, `_poudrine`. |
| `upstream`, `fingerprints` | FreeBSD latest for the host's package ABI, `/usr/share/keys/pkg`. |
| `cache_gb`, `refresh` | 5 GB of download cache, five minutes between FreeBSD catalog checks. Completing a build triggers an additional check. To update an installation with an explicit hourly interval, set `refresh` to `"5m"` or change **Catalog refresh interval** in Management. |
| `jail`, `ports`, `jobs`, `build_interval` | `latest`, `main`, six parallel jobs, 15 minutes between scheduled runs. If your configuration already specifies a longer interval, set **Poudriere builds → Build interval** to `15m` in Service settings. |
| `working_hours` | Daily working hours, for example `"22:00-06:00,11:00-17:00"`. Defaults to unrestricted. |
| `package_overlays` | Per-port build settings. Defaults to an empty object. See [Build a package with your own options](#build-a-package-with-your-own-options). |
| `make_jobs_number`, `make_jobs_number_limit` | Per-package make job count and cap. Each defaults to `0`, which leaves that override unset. A count or cap above one enables parallel make when the other setting permits it. |
| `local_repo`, `packages_file` | Poudriere’s published package directory and `/usr/local/etc/poudriere.d/packages.txt`. |

- **TLS.** For a certificate issued by your local CA, give the service account read access to the PEM chain and matching unencrypted PEM private key. Run `poudrine set-tls -tls-cert /path/to/fullchain.pem -tls-key /path/to/privkey.pem` as root. Supply `-config /path/to/poudrine.json` after `set-tls` for a custom configuration. Restart after renewing files in place. Use `poudrine set-tls -self-signed` to restore automatic certificates. Verify the fingerprint again after an automatic renewal or hostname change.
- **Private reverse proxy.** Bind backends to loopback, serve clients an HTTPS repository URL and restrict incoming connections to your LAN or VPN. To preserve client attribution, set `trusted_proxy` to the exact proxy address (`/32` or `/128`) and overwrite `X-Real-IP` at that proxy. For a website mounted under a path, include the path in `web_url` and strip the prefix before forwarding.
- **Upgrade during a build.** Install the updated binaries from the new release or checkout using the [installation steps](installation.md), then run `service poudrine restart` as root. The service pauses Poudriere and its compilers while the updated web and repository servers start. The same processes then continue the package they were building. A build outside working hours stays paused until its window opens. The supervisor switches to the new binary after the run finishes and carries over queued builds and retries. If you use the optional status reader, run `service poudrine_status restart` separately.
- **First upgrade from an older release.** A supervisor installed before restart support needs one ordinary stop and start. Let the active build finish, then run `service poudrine stop`, `make install`, and `service poudrine start`. The new rc script reports when this one-time step is needed. An explicit stop or a host shutdown still terminates an active build.
- **Move the service.** Stop it before copying its private state directory, including `inventory.sqlite`, `repository.key` and the TLS files. Preserve the state directory’s service-account ownership and mode 0700. Keep the root-owned configuration, `<config>.admin.pub` and worker files, including the worker build signing key. A new state directory creates a new repository signing identity, which requires distributing a new public key to clients.
- **Sign in with a saved browser key.** If you enrolled with an older version, open Management in the original browser profile at the same scheme, hostname and port. Choose **Use saved browser key**.

### Signed management API

API reads use the custom HTTP method `QUERY`. Send arguments in a JSON object and use `{}` for a read without arguments. API URLs reject query strings. Configure any reverse proxy to forward `QUERY`, the body and signature headers. Ordinary page visits and FreeBSD package downloads use standard HTTP.

Management requests use the enrolled administrator’s Ed25519 key. To sign a request, concatenate this UTF-8 prefix and the exact bytes of the body you will send. Each `\n` represents one LF byte. Use the escaped route path as received by Poudrine after any proxy prefix removal.

```text
poudrine-v2\n<METHOD>\n<PATH>\n<UNIX_SECONDS>\n<NONCE>\n
```

| Header | Value |
| --- | --- |
| `X-Poudrine-Time` | Decimal Unix time in seconds, within five minutes of the server clock. |
| `X-Poudrine-Nonce` | Fresh 16-byte random value, unpadded base64url. Reuse of an accepted nonce returns HTTP 409. |
| `X-Poudrine-Signature` | The Ed25519 signature, unpadded base64url. |
| `X-Poudrine-Key` | Raw Ed25519 public key, unpadded base64url. Include it for initial enrollment and client inventory reports. Management looks up its enrolled key for subsequent requests. |

| Request | Example signed body |
| --- | --- |
| `POST /api/admin/login` | `{}` |
| `QUERY /api/admin/build` | `{}` |
| `QUERY /api/admin/build/events` | `{}` |
| `QUERY /api/admin/build/history` | `{"interval":"day"}` |
| `QUERY /api/admin/build/history/packages` | `{"interval":"day","start":"2026-11-21T00:00:00Z","as_of":"2026-11-21T17:45:00Z"}` |
| `QUERY /api/admin/build/results` | `{}` for the latest retained run, `{"hide_empty":true}` for the latest run with results, or `{"started":"2026-11-21T02:00:00.123456789Z"}` for a specific run |
| `QUERY /api/admin/build/issues` | `{}` for the latest run with issues, or `{"started":"2026-11-21T02:00:00.123456789Z"}` |
| `QUERY /api/admin/build/log` | `{"started":"2026-11-21T02:00:00.123456789Z","package":"abseil-20250127.1_3"}` |
| `POST /api/admin/build/retry` | `{"started":"2026-11-21T02:00:00.123456789Z","package":"abseil-20250127.1_3"}` |
| `QUERY /api/admin/clients/packages` | `{"ip":"2001:db8::10"}` |
| `POST /api/admin/build/force` | `{}` |

Login returns HTTP 204. Each `build` event contains the full snapshot, starting with the current state when you connect. Sign each reconnection with a fresh nonce. History accepts `hour`, `day`, `week` and `month`. Forcing a build returns HTTP 202 on acceptance and includes `already_queued` in the response. The client report command on the website signs the exact text it posts to the repository’s `/api/inventory` with that client’s own key.

In a build snapshot, each running job can include `progress_percent` from its build tool, `active_elapsed_seconds` from the worker, and `last_build_seconds` from build history. Subtract active elapsed seconds from the previous duration to estimate the remaining build time. A negative value gives the time past the estimate.

Use the exact `started` value returned by results or history to select a run. The results response lists retained `runs` newest first and maps their start times to successful package counts in `compiled`. An unreadable run can lack a count. The log query returns the newest output first. Pass its `start` byte offset as `before` to read the preceding chunk.

A retry with `started` and `package` queues that package and its affected dependency group. Send `started` with `"outcome":"failed"` or `"outcome":"skipped"` to queue the ports recorded under that outcome. Sending only `started` queues both groups.

To list packages for a chart bar, send the history response’s `interval` and `as_of` together with that bucket’s `start`. Each page contains up to 50 attempts with the package version, outcome and timestamps. When the response includes `next`, send it as `after` in the next signed query with the same period.

### Optional public status page

To publish just the package count and the number ahead of FreeBSD, run `sysrc poudrine_status_enable=YES` and `service poudrine_status start` as root. The separate service listens on `[::]:8282` by default and serves public HTML at `/`. For JSON, use `GET /status.json`; `HEAD` returns the same headers without a body. Both the HTML page and JSON endpoint are public and require no keys, signatures or request body. Set `poudrine_status_listen` in `rc.conf` to choose another address.

