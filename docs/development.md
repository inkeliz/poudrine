# Development


Run `CGO_ENABLED=0 go test ./...` and `make build` beside `go.mod`. The two commands live in `cmd/poudrine` and `cmd/poudrine-status`. Each embeds its own files from the corresponding directory under `web/`. Run `make dist` to create binary and source archives in `dist/`.

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
