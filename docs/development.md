# Development


Poudrine targets FreeBSD only. Run `make test` (which runs `CGO_ENABLED=0 go test ./...`) and `make build` on FreeBSD beside `go.mod`. The two commands live in `cmd/poudrine` and `cmd/poudrine-status`. Their implementations embed the files under `internal/web/`. Run `make dist` to create binary and source archives in `dist/`.

Application code lives under `internal/`:

- `service` wires the commands, processes, configuration reloads and restarts together.
- `admin` serves management APIs; `broker` authenticates requests forwarded to the privileged supervisor.
- `public` serves packages, client setup and inventory reports. Its `status` subpackage implements the separate, confined status reader.
- `builder` owns Poudriere execution, scheduling, progress and native build logs.
- `catalog` owns signed catalogs, live publication, downloads, caching and package inventory views.
- `database` owns SQLite connections, schemas, migrations and queries, grouped by table or responsibility. Other packages use its typed operations.
- `config` owns settings and validation; `packageinfo`, `requestauth` and `fsutil` contain shared package metadata, signing protocol and file operations.

Tests live with the package they exercise. Cross-package HTTP and worker tests use the same database, broker and service interfaces as production. Worker-launching test binaries retain the command-helper entry point required by the FreeBSD reaper.

CI runs the test suite as an unprivileged user in a FreeBSD/amd64 VM with Poudriere installed. It also runs the service restart test as root to check that an in-progress build resumes. A release requires both the native test job and the archive build to pass.

Push a tag such as `v1.2.3` to publish the FreeBSD/amd64 binary archive and the source archive. A tag such as `v1.2.3-rc.1` publishes a prerelease. Branch pushes and pull requests also build both archives. [GitHub Releases](https://github.com/inkeliz/poudrine/releases)

On FreeBSD, set `POUDRINE_TEST_PORTSDIR=/usr/ports` when running the tests to check the generated overlay against a local ports checkout. The test resolves mydumper’s MariaDB dependency using the ports framework.

To exercise the HTTP services alone, save the following in `dev.json` and run `./poudrine serve -config ./dev.json` as an ordinary user.

```json
{
  "state": "./dev-state",
  "repo_listen": "[::1]:8080",
  "web_listen": "[::1]:8181"
}
```

Open `https://localhost:8181/`. Standalone mode creates its automatic certificate and enrollment token in `dev-state/`. The full FreeBSD service runs the Poudriere worker and enables manual builds. The `-config` startup option selects the JSON file for `run`, `serve` and `worker`.
