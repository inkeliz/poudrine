# Run on FreeBSD


Use a FreeBSD RELEASE host that can create jails and reach the upstream FreeBSD package and ports servers. Allow storage for the jail, ports checkout, distfiles, build directories and published packages.

### 1. Install the binary

On a FreeBSD/amd64 server, download and extract the latest binary archive, then run `make install` as root. To choose a specific version, visit [GitHub Releases](https://github.com/inkeliz/poudrine/releases).

```sh
fetch -o poudrine-freebsd-amd64.tar.gz https://github.com/inkeliz/poudrine/releases/latest/download/poudrine-freebsd-amd64.tar.gz
tar -xzf poudrine-freebsd-amd64.tar.gz
cd poudrine
make install
```

To compile on FreeBSD, install Go 1.26 or later with `pkg install go`. Use a Git checkout or extract the latest source archive. In the directory containing `go.mod`, `make install` builds the binaries from the current source before installing them. To compile as an ordinary user, run `make build`, then run `make install-built` as root from the same directory. The main binary embeds the website and uses SQLite without CGO. [Download the source archive](https://github.com/inkeliz/poudrine/releases/latest/download/poudrine-src.tar.gz)

### 2. Bind the service to your network

As root, create `/usr/local/etc/poudrine.json`. In this example, `fd42:10::20` is the server’s private LAN address and `pkg.home.arpa` resolves to it on every client. Substitute an address assigned to your server and your own LAN DNS name. Set `repo_url` to the full address clients will use, including the repository port.

```json
{
  "repo_listen": "[fd42:10::20]:8080",
  "repo_url": "http://pkg.home.arpa:8080",
  "web_listen": "[::1]:8181"
}
```

An IPv4 LAN can use a listener such as `192.168.10.20:8080`. For a single machine or access over SSH tunnels, the [bundled configuration](../examples/poudrine.json) binds both listeners to IPv6 loopback. A port alone, such as `"9080"`, binds all IPv6 interfaces. Keep the configuration file and its parent directory root-controlled. For a different file location, set `sysrc poudrine_config=/path/to/poudrine.json` and use that path for the commands below.

```sh
poudrine check-config -config /usr/local/etc/poudrine.json
sysrc poudrine_enable=YES
service poudrine start
service poudrine status
```

On the first start Poudrine creates the `_poudrine` service account, installs missing Poudriere and Git packages, generates signing and website keys, and provisions a non-ZFS build jail and ports tree. An upgrade from the old rc.conf-based version imports the previous options once when the JSON file is missing. The website can become available before the first catalog. Repository metadata returns HTTP 503 until Poudrine has a verified catalog to serve. Check `/var/log/messages` for provisioning or catalog errors. To disable automatic startup, use `sysrc poudrine_enable=NO`.

### 3. Connect to the website and enroll the administrator

From your workstation, forward the loopback website port over SSH. Open `https://localhost:8181/` through the tunnel.

```sh
ssh -N -L '8181:[::1]:8181' admin@buildhost
```

On the server console or through trusted SSH, run these commands as root. Compare the SHA-256 fingerprint with the self-signed certificate shown by your browser before trusting the website or copying client setup commands.

```sh
openssl x509 -in /var/db/poudrine/web-tls.crt -noout -sha256 -fingerprint
cat /usr/local/etc/poudrine.json.setup-token
```

1. Open **Management** and enter the one-time token. The browser creates an Ed25519 administrator key. Save the **entire private key**, including the PEM header and footer, in your password manager or a private backup.
2. Choose **I’ve saved my key · Sign in**, then paste or autofill the key in the password box. If your password manager asks for a username, use `admin`. Poudrine signs requests in the browser and stores the public key in `<config>.admin.pub`.
3. Open **Service settings → Network** and check the prefilled **Package repository URL** from a client’s point of view. Choose **Done** to keep the defaults or **Save settings** to apply edits. Automatic values continue to follow the host and configuration until you override them.

