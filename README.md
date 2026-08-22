# cfd-client — IDA ICE ↔ CFD backend client

`cfd-client` moves CFD case data between IDA ICE and the EQUA **cfd-backend**.
IDA ICE (whose Lisp has no general HTTP client) calls it as a subprocess. It is
a single **static, dependency-free** executable (Go) that targets the
file-server endpoints under `/api/rw` plus a few case endpoints, and reads the
server URL from a JSON config file so the same binary works locally or against a
remote server.

It's compiled rather than scripted for **startup latency**: IDA ICE invokes the
client once per backend interaction, and the binary starts in ~2 ms versus the
~100–200 ms an interpreter would cost — a gap that is largest on Windows.

> These files are synced into the IDA ICE (Windows) application source tree;
> ship the built binary (and `cfd_client_config.json`) alongside the app.

## Build

Needs only the Go toolchain — no C compiler, and cross-compiling needs no target
machine:

```
make build      # native binary: ./cfd-client
make dist       # dist/cfd-client.exe (Windows) + dist/cfd-client-linux-amd64
```

Both `dist` targets are static (`CGO_ENABLED=0`) and stripped (~5.7 MB). Other
platforms are one `GOOS`/`GOARCH` away, e.g.
`GOOS=linux GOARCH=arm64 go build -o cfd-client .`.

On Windows without `make`, use the PowerShell script — it builds
`cfd-client.exe` and smoke-tests it (--help, config, and the error contract):

```powershell
./build.ps1
```

Everything below uses `cfd-client`; on Windows it is `cfd-client.exe`.

## Configuration

`cfd_client_config.json`, searched in this order: `--config <path>`, the
`CFD_CLIENT_CONFIG` env var, next to the executable, then the current dir.

The config holds one or more **named backends**; `--host <tag>` selects one,
otherwise `default_backend` is used. Other top-level keys (e.g. `wsl`) are
ignored by this tool.

```json
{
    "default_backend": "vmhost",
    "backends": {
        "vmhost": {
            "server_url":   "http://192.168.122.1:5001",
            "frontend_url": null,
            "timeout":      600,
            "verify_tls":   true
        },
        "wsl":    { "server_url": "http://localhost:5001" },
        "server": { "server_url": "http://10.228.20.136:5001" }
    }
}
```

Per-backend keys:

- `server_url` — base URL of the server that serves `/api/rw` (a bare
  `host:port` is assumed to be `http://`). All-in-one deployments use port 5001;
  a standalone file server uses 5002.
- `frontend_url` — the cfd-frontend to open in a browser; defaults to `server_url`.
- `timeout` — seconds per request (case zips can be large — keep it generous).
- `verify_tls` — set `false` only for an internal server with a self-signed cert.

Selection:

- `--host <tag>` picks a backend from `backends`; omit it to use `default_backend`.
- `--server-url <url>` overrides the resolved backend's `server_url` for a one-off
  call (and lets the client run even without a matching backend entry).
- `config` dumps the resolved backend (handy to confirm which host is active):
  `cfd-client --host server config`.

## CLI

Every command prints **one JSON object** to stdout and sets the exit code
(`0` success, `1` failure), so Lisp can both branch on the code and parse the
result (the exception is `--progress`, which prepends `{"progress": …}` lines —
see below; the last line is still this result object). All human/diagnostic text
goes to stderr, so stdout can be parsed as JSON unconditionally:

```jsonc
{"ok": true,  "exit": 0, "result": { ... }}
{"ok": false, "exit": 1, "error": "download: not found on server: abc.zip"}
```

Global flags (before the command): `--config <path>`, `--host <tag>`,
`--server-url <url>`, `--progress`.

**Missing items read consistently.** A lookup for something that isn't there —
`ls <path>`, or `ls-fs`/`ls-wd` with a name — succeeds with an **empty result**
(`{"ok": true, "exit": 0, "result": {}}`), not an error: the query ran, the
answer is "nothing there". Genuine failures (server unreachable, 5xx, a bad
request) still exit 1. (`exists` is the boolean form of the same idea.)

```sh
cfd-client [--host TAG] upload    <local_path> [--name REMOTE]  # file, or dir (zipped)
cfd-client [--host TAG] download  <remote_name> <local_dest>    # dest file or dir
cfd-client [--host TAG] ls        <PATH>   # any path under CFD_HOME (/api/ls): dir contents, or a file
cfd-client [--host TAG] ls-fs     [NAME]   # transfer area (/api/rw): all files, or one file's info
cfd-client [--host TAG] ls-wd     [CASE]   # working dirs: all cases, or one case's contents
cfd-client [--host TAG] metadata  <CASE>   # full case metadata (IDA fields + case_info: time, ncells)
cfd-client [--host TAG] newest    <STEM>   # info of newest transfer-area file sharing a stem
cfd-client [--host TAG] exists    <remote_name>
cfd-client [--host TAG] rm        <remote_name>
cfd-client [--host TAG] upstage   <remote_name> [--wd <name-or-path>]  # stage server-side
cfd-client [--host TAG] ensure    <case-id> [--force]  # resume: ensure staged (least work)
cfd-client [--host TAG] downstage <case_path>                          # pack + publish for download
cfd-client [--host TAG] url       [--open]  # print / open the cfd-frontend
cfd-client [--host TAG] config              # dump the resolved backend config
cfd-client [--host TAG] cleanup             # server purges old files
```

Example round trip (upload a case, stage it, run it, fetch results):

```sh
cfd-client upload    C:\ida\mycase --name 3f9c-case.zip  # zips the dir
cfd-client upstage   3f9c-case.zip --wd 3f9c-case        # unpack server-side
#   ... run setup/mesh/solve via the backend's other /api/* endpoints ...
cfd-client downstage 3f9c-case                           # pack + publish
cfd-client download  3f9c-case.downstage C:\ida\results\ # fetch the result
```

`upstage` tells the backend to fetch the already-uploaded file and unpack it into
a working directory `--wd` (name or path-like string, under the server's
CFD_HOME; defaults to the remote name's stem). The file's URL is inferred from
`remote_name` and this client's `server_url` (`<server_url>/api/rw/<remote_name>`).
It is sent as a POST so the reply is always JSON (a GET would redirect to the
cockpit page). The backend starts the unpack as a background job and replies
`{"status": "started", "case_path": ...}` — it does not wait for completion.

> **`server_url` must be reachable *from the backend*, not just from you.**
> upstage hands the backend that URL to fetch, so it has to resolve from inside
> the backend as well. A browser-style `http://localhost:8080` is the classic
> trap: `localhost` inside the backend container is the container itself, and
> `:8080` is the nginx port, which isn't open there — the fetch fails with
> "backend could not reach the uploaded file". Use an address that works from
> both sides — the host's LAN/bridge IP (e.g. `http://192.168.122.1:8080`), or
> the backend's own service port (`:5001`) when the client runs on the host. A
> plain `upload`/`download`/`ls` is unaffected (only the client talks to the
> server); it's specifically upstage's backend-initiated fetch that needs this.

`ensure` is the **resume** entry point: it makes a case usable on the backend
with the least work, so you don't re-upload what the server already has. The
backend replies (as `result`):
- `{"status": "ready", "state": ...}` — already staged, or just staged from the
  file server (no upload); open the SPA.
- `{"status": "need_upload"}` — the server has nothing for this case; `upload`
  it, then call `ensure` again.

The backend finds the case on the file server itself (it knows its own file
server), so — unlike `upstage` — you pass **no URL**, only the case-id, and the
reachability caveat above doesn't apply. `--force` re-stages from the file-server
upload even when a copy is already staged (a clean replace) — for when you know a
*different* backend holds the newer copy and this one's is stale; you own that
call. A typical resume is one call: `cfd-client ensure <case-id>` → if
`need_upload`, `upload` then `ensure` again.

`downstage` is the reverse: it packs the processed case at `case_path`
(reconstruct + zip) and publishes it to the server's file store as
`<name>.downstage` — the `.downstage` suffix marks a processed case, distinct
from an uploaded one. It runs **synchronously** and replies
`{"status": "published", "name": "<case>.downstage", "url": ...}`; download that
`name` to retrieve the result. The publish target is derived from the request
host, so in an all-in-one deployment the backend uploads to its own `/api/rw`.

### Progress reporting (`--progress`)

Case zips can be large, so `upload` and `download` can stream progress for a
progress bar. With `--progress`, they print throttled **`{"progress": <pct>}`**
lines to stdout *before* the single terminal result line (the output becomes
newline-delimited JSON):

```jsonc
{"progress": 0}
{"progress": 5}
   ...
{"progress": 100}
{"ok": true, "exit": 0, "result": { ... }}   // always the last line
```

Percent is emitted in steps of ~5 % (never the same value twice, always a final
`100`), then the outcome. A reader treats a line containing `"progress"` as a
bar update and the line containing `"ok"` as the final result. Without
`--progress` the tool prints the single result object, so the flag is a no-op for
every other command.

## Notes on the backend contract

- **Upload** sends the file as the **raw request body** (not multipart), always
  with a `Content-Length` (a sized, non-chunked body), which the server requires.
- If the upload path is a **directory**, it is zipped first (contents at the
  archive root, matching the IDA ICE case-zip layout) to a temporary file and
  uploaded as `<dir>.zip` (override with `--name`); the temp zip is then removed.
- The server sanitises the URL name and stores files **flat** in its upload
  folder, so use plain names like `<uuid>.zip` — path separators are flattened
  server-side.
- **Download** streams to a `*.part` temp then renames, so a partial transfer
  never leaves a truncated file in place. Percent (with `--progress`) is derived
  from the response `Content-Length`; a length-less (chunked) reply just yields
  no mid-transfer progress lines.

## Using it from Lisp (pattern)

Run the executable, capture stdout, check the exit code, and parse the JSON:

```
run:  cfd-client --config <cfg> upload <zip> --name <id>.zip
ok:   exit 0  → parse result.uploaded / result.bytes
fail: exit 1  → read error string from the JSON
```

Uploading the case zip is only the first step; triggering setup/solve and
reading run status use the backend's other `/api/*` endpoints (see the backend
`api.yml`), which can be added to this tool as needed.

## Source layout

- `main.go` — CLI parsing, dispatch, the JSON/exit-code output contract.
- `client.go` — the HTTP client and its operations.
- `config.go` — config file discovery and backend resolution.
