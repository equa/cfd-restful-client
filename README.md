# IDA ICE ↔ CFD backend client tools

`ida_cfd_client.py` moves CFD case data between IDA ICE and the EQUA
**cfd-backend**. IDA ICE (whose Lisp has no general HTTP client) calls it as a
subprocess; it can also be imported as a module. It targets the file-server
endpoints under `/api/rw` and reads the server URL from a JSON config file so the
same tool works locally or against a remote server.

> Not part of the cfd-backend git repo — these files belong in the IDA ICE
> (Windows) application source tree.

## Requirements

- Python 3.8+
- [`requests`](https://pypi.org/project/requests/) — `pip install requests`
  (or bundle it with the IDA ICE Python runtime)

## Configuration

`cfd_client_config.json`, searched in this order: `--config <path>`, the
`CFD_CLIENT_CONFIG` env var, next to `ida_cfd_client.py`, then the current dir.

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
  `ida_cfd_client.py --host server config`.

## CLI

Every command prints **one JSON object** to stdout and sets the exit code
(`0` success, `1` failure), so Lisp can both branch on the code and parse the
result (the exception is `--progress`, which prepends `{"progress": …}` lines —
see below; the last line is still this result object):

```jsonc
{"ok": true,  "result": { ... }}
{"ok": false, "error": "download: not found on server: abc.zip"}
```

Global flags (before the command): `--config <path>`, `--host <tag>`,
`--server-url <url>`, `--progress`.

### Progress reporting (`--progress`)

Case zips can be large, so `upload` and `download` can stream progress for a
progress bar. With `--progress`, they print throttled **`{"progress": <pct>}`**
lines to stdout *before* the single terminal result line (i.e. the output
becomes newline-delimited JSON):

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
`--progress` the tool prints the single result object as before, so the flag is
a no-op for every other command.

```sh
python ida_cfd_client.py [--host TAG] upload   <local_path> [--name REMOTE]  # file, or dir (zipped)
python ida_cfd_client.py [--host TAG] download <remote_name> <local_dest>   # dest file or dir
python ida_cfd_client.py [--host TAG] ls       <PATH>  # any path under CFD_HOME (/api/ls): dir contents, or a file
python ida_cfd_client.py [--host TAG] ls-fs    [NAME]  # transfer area (/api/rw): all files, or one file's info
python ida_cfd_client.py [--host TAG] ls-wd    [CASE]  # working dirs: all cases, or one case's contents
python ida_cfd_client.py [--host TAG] metadata <CASE>  # full case metadata (IDA fields + case_info: time, ncells)
python ida_cfd_client.py [--host TAG] newest   <STEM>  # info of newest transfer-area file sharing a stem
python ida_cfd_client.py [--host TAG] exists   <remote_name>
python ida_cfd_client.py [--host TAG] rm       <remote_name>
python ida_cfd_client.py [--host TAG] upstage  <remote_name> --wd <name-or-path>  # stage server-side
python ida_cfd_client.py [--host TAG] downstage <case_path>                       # pack + publish for download
python ida_cfd_client.py [--host TAG] url      [--open]     # print / open the cfd-frontend
python ida_cfd_client.py [--host TAG] config               # dump the resolved backend config
python ida_cfd_client.py [--host TAG] cleanup              # server purges old files
```

Example round trip (upload a case, stage it, run it, fetch results):

```sh
python ida_cfd_client.py upload   C:\ida\mycase --name 3f9c-case.zip  # zips the dir
python ida_cfd_client.py upstage  3f9c-case.zip --wd 3f9c-case        # unpack server-side
#   ... run setup/mesh/solve via the backend's other /api/* endpoints ...
python ida_cfd_client.py downstage 3f9c-case                          # pack + publish
python ida_cfd_client.py download 3f9c-case.downstage C:\ida\results\ # fetch the result
```

`upstage` tells the backend to fetch the already-uploaded file and unpack it into
a working directory `--wd` (name or path-like string, under the server's
CFD_HOME). The file's URL is inferred from `remote_name` and this client's
`server_url` (`<server_url>/api/rw/<remote_name>`); if the file server and
backend are on different hosts, the backend must be able to reach that URL.
It is sent as a POST so the reply is always JSON (a GET would redirect to the
cockpit page). The backend starts the unpack as a background job and replies
`{"status": "started", "case_path": ...}` — it does not wait for completion.

`downstage` is the reverse: it packs the processed case at `case_path`
(reconstruct + zip) and publishes it to the server's file store as
`<name>.downstage` — the `.downstage` suffix marks a processed case, distinct
from an uploaded one. It runs **synchronously** and replies
`{"status": "published", "name": "<case>.downstage", "url": ...}`; download that
`name` to retrieve the result. The publish target is derived from the request
host, so in an all-in-one deployment the backend uploads to its own `/api/rw`.

## Notes on the backend contract

- **Upload** sends the file as the **raw request body** (not multipart); the
  tool always sends a sized body (`Content-Length`), which the server requires.
- If the upload path is a **directory**, it is zipped first (contents at the
  archive root, matching the IDA ICE case-zip layout) to a temporary file and
  uploaded as `<dir>.zip` (override with `--name`); the temp zip is then removed.
- The server sanitises the URL name and stores files **flat** in its upload
  folder, so use plain names like `<uuid>.zip` — path separators are flattened
  server-side.
- **Download** streams to a `*.part` temp then renames, so a partial transfer
  never leaves a truncated file in place.
- With `--progress`, upload still sends a sized body: the file is wrapped so
  `requests` keeps the required `Content-Length` while reporting read progress
  (no chunked transfer-encoding). Download derives percent from the response
  `Content-Length`; a length-less (chunked) reply just yields no mid-transfer
  progress lines.

## Using it from Lisp (pattern)

Run the executable, capture stdout, check the exit code, and parse the JSON:

```
run:  python ida_cfd_client.py --config <cfg> upload <zip> --name <id>.zip
ok:   exit 0  → parse result.uploaded / result.bytes
fail: exit 1  → read error string from the JSON
```

Uploading the case zip is only the first step; triggering setup/solve and
reading run status use the backend's other `/api/*` endpoints (see the backend
`api.yml`), which can be added to this tool later as needed.

## Compiled client (Go)

`ida_cfd_client.py` has a drop-in Go port (`main.go`, `client.go`, `config.go`)
with the **same CLI and the same output contract** — a subprocess caller needs
no changes. The motivation is startup latency: IDA ICE invokes the client once
per backend interaction, and the Go binary starts in ~2 ms versus ~100–200 ms
for the Python interpreter.

- **Dependency-free**: standard library only (no `requests`, no runtime to
  install) — a single static executable.
- **Same config**: reads the same `cfd_client_config.json`, same search order,
  same `--config/--host/--server-url/--progress` flags and subcommands.
- **Same stdout contract**: one JSON object per call (`{"ok",...}`), throttled
  `{"progress": n}` lines during transfers, exit 0/1, diagnostics on stderr.

Build (needs only the Go toolchain — no C compiler, no target machine):

```
make build      # native binary: ./cfd-client
make dist       # dist/cfd-client.exe (Windows) + dist/cfd-client-linux-amd64
```

Both `dist` targets are static (`CGO_ENABLED=0`) and stripped (~5.7 MB). Other
platforms are one `GOOS`/`GOARCH` away, e.g.
`GOOS=linux GOARCH=arm64 go build -o cfd-client .`.

### Status: Go is the maintained client

The project is standardising on the **Go client** — the compiled binary spawns
far faster than the Python interpreter, most noticeably on Windows, where IDA
ICE invokes it per interaction. New work goes into the Go client.

`ida_cfd_client.py` is kept for reference/fallback but is **no longer maintained
in lockstep** — it may lag the Go client and can be removed once the Go binary
is deployed in the IDA ICE tree.
