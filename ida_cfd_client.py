#!/usr/bin/env python3
"""IDA ICE <-> CFD backend file-transfer client.

A small, dependency-light client (Python 3 + `requests`) that IDA ICE can call
as a subprocess to move CFD case data to/from the EQUA cfd-backend. It targets
the file-server endpoints under ``/api/rw`` and reads the server URL from a JSON
config file so the same executable works against a local server
(``http://localhost:5001``) or a remote one (``https://myserver.se``).

Backend contract (cfd-backend ``routes/fileserver.py``), all under the base URL:

    POST   /api/rw/<name>      upload  - the request BODY is the raw file bytes
                                         (NOT multipart); a Content-Length is
                                         required, so we always send a sized body
    GET    /api/rw/<name>      download - response body is the raw file bytes,
                                         or 404 JSON if absent
    DELETE /api/rw/<name>      delete   - JSON {"message": ...}
    GET    /api/rw/ls          list     - JSON list of {name, path, file_size,
                                         mtime, last_modified, file_type}
    GET    /api/rw/ls/<name>   stat     - the single matching item dict, or {}
    GET    /api/rw/newest/<stem>        info of the newest file sharing a name
                                         stem (file-a.zip vs file-a.downstage),
                                         or {} if none match
    GET    /api/rw/cleanup_folders      remove files older than the server's TTL

The server sanitises the URL name (werkzeug ``secure_filename``) and stores files
flat in its upload folder, so use plain names such as ``<uuid>.zip`` (path
separators are flattened server-side).

CLI (designed for subprocess use from IDA ICE Lisp): **stdout carries only a
single JSON object** (one line) and the exit code is set (0 = success,
1 = failure) - including on missing-dependency and bad-argument errors - so the
caller can parse stdout as JSON unconditionally and also branch on the code. All
human-readable / diagnostic text and library warnings go to stderr, never
stdout. (The sole exception is ``-h``/``--help``, which prints human help.)

    python ida_cfd_client.py [--config PATH] [--host TAG] [--server-url URL] <command> ...
      upload   <local_path> [--name REMOTE]   # a file, or a directory (zipped)
      download <remote_name> <local_dest>
      ls         <PATH>          # any path under CFD_HOME (/api/ls): dir contents, or a file
      ls-fs      [NAME]          # transfer area (/api/rw): all files, or one file's info
      ls-wd      [CASE]          # working dirs: all cases, or one case's contents
      metadata   <case>          # full case metadata (IDA fields + case_info: time, ncells)
      newest     <stem>          # info of the newest transfer-area file with this stem
      exists   <remote_name>
      rm       <remote_name>
      upstage  <remote_name> --wd <name-or-path>   # stage an uploaded file server-side
      downstage <case_path>                        # pack a processed case, publish for download
      url      [--open]        # print (or open) the cfd-frontend URL
      config                   # dump the resolved backend config
      cleanup

Config file (JSON), searched in order: --config, $CFD_CLIENT_CONFIG,
``cfd_client_config.json`` next to this script, then the current directory.
It holds one or more named backends; ``--host <tag>`` picks one, else
``default_backend`` is used. Other top-level keys (e.g. ``wsl``) are ignored.

    {
        "default_backend": "vmhost",
        "backends": {
            "vmhost": {
                "server_url":   "http://192.168.122.1:5001",
                "frontend_url": null,     // optional; defaults to server_url
                "timeout":      600,      // seconds per request
                "verify_tls":   true      // set false for internal self-signed certs
            },
            "wsl": { "server_url": "http://localhost:5001" }
        }
    }

``--server-url URL`` overrides the resolved backend's server_url (and lets the
client run without a matching backend entry).
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
import tempfile
import zipfile
from pathlib import Path
from urllib.parse import quote, urlsplit, urlunsplit

try:
    import requests
except ImportError:  # emit JSON so a stdout-parsing caller still gets structure
    _m = "the 'requests' module is required (pip install requests)"
    sys.stderr.write(_m + "\n")  # human hint on stderr
    json.dump({"ok": False, "error": _m}, sys.stdout)  # machine result on stdout
    sys.stdout.write("\n")
    sys.exit(1)


CONFIG_BASENAME = "cfd_client_config.json"
DEFAULT_TIMEOUT = 600
CHUNK = 1 << 16  # 64 KiB streaming chunk
PROGRESS_STEP = 5  # emit {"progress": pct} at most every 5 % during a transfer


def _make_progress_emitter(step: int = PROGRESS_STEP):
    """Return a ``callback(pct)`` that writes throttled ``{"progress": <int>}``
    NDJSON lines to stdout (each flushed immediately, so a subprocess caller can
    move a progress bar in real time).

    It emits the very first call, then only when the integer percent has
    advanced by ``step`` or reached 100 - never the same value twice. These
    lines are written *before* the single terminal result line from ``_emit``,
    so a reader treats a line with ``"progress"`` as an update and the line with
    ``"ok"`` as the final outcome."""
    last = -1

    def emit(pct) -> None:
        nonlocal last
        pct = max(0, min(100, int(pct)))
        if last < 0 or pct - last >= step or (pct == 100 and last != 100):
            last = pct
            json.dump({"progress": pct}, sys.stdout)
            sys.stdout.write("\n")
            sys.stdout.flush()

    return emit


class _ProgressReader:
    """Read-through file wrapper that reports upload progress.

    ``requests`` derives Content-Length from ``len(body)`` and then streams a
    file-like body by calling ``read`` repeatedly; we expose ``__len__`` so the
    raw-body upload keeps the Content-Length the server requires, and forward
    each read's running total to the progress callback."""

    def __init__(self, fh, total: int, progress):
        self._fh = fh
        self._total = total
        self._read = 0
        self._progress = progress

    def __len__(self) -> int:
        return self._total

    def read(self, size=-1) -> bytes:
        chunk = self._fh.read(size)
        if chunk:
            self._read += len(chunk)
            if self._total > 0:
                self._progress(self._read * 100 / self._total)
        return chunk


class CfdClientError(Exception):
    """A transfer or protocol error worth reporting to the caller."""


# --------------------------------------------------------------------------- #
# Config
# --------------------------------------------------------------------------- #


def _normalise_url(url: str) -> str:
    """Return a clean base URL: add a scheme if missing, strip a trailing '/'."""
    url = (url or "").strip()
    if not url:
        raise CfdClientError("server_url is empty")
    if "://" not in url:
        url = "http://" + url  # bare host:port -> http
    parts = urlsplit(url)
    return urlunsplit((parts.scheme, parts.netloc, parts.path.rstrip("/"), "", ""))


def find_config(explicit: str | None) -> Path | None:
    """Locate the config file (explicit path, env var, alongside script, CWD)."""
    candidates = []
    if explicit:
        candidates.append(Path(explicit))
    if os.environ.get("CFD_CLIENT_CONFIG"):
        candidates.append(Path(os.environ["CFD_CLIENT_CONFIG"]))
    candidates.append(Path(__file__).resolve().parent / CONFIG_BASENAME)
    candidates.append(Path.cwd() / CONFIG_BASENAME)
    for c in candidates:
        if c.is_file():
            return c
    return None


def load_config(
    explicit: str | None,
    server_url_override: str | None,
    host: str | None = None,
) -> dict:
    """Resolve the backend config to use from the JSON config file.

    Config format::

        {
          "default_backend": "<tag>",
          "backends": {
            "<tag>": {"server_url": ..., "frontend_url": ...,
                      "timeout": ..., "verify_tls": ...},
            ...
          },
          ...   # other top-level keys (e.g. "wsl") are ignored by this app
        }

    The backend is chosen by ``host`` (the --host tag) or, if omitted, by
    ``default_backend``.  --server-url overrides the resolved server_url and, if
    no valid backend is selectable, lets the client run config-lessly.  Returns
    the chosen backend dict, tagged with the resolved ``backend`` name.
    """
    cfg: dict = {}
    path = find_config(explicit)
    if path is not None:
        try:
            cfg = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as e:
            raise CfdClientError(f"cannot read config {path}: {e}")

    backends = cfg.get("backends", {})
    host_tag = host or cfg.get("default_backend")

    if host_tag and host_tag in backends:
        backend = dict(backends[host_tag])
    elif server_url_override:
        # No resolvable backend, but an explicit URL was given — run with it.
        backend = {}
    elif host_tag:
        available = ", ".join(sorted(backends)) or "(none)"
        raise CfdClientError(
            f"unknown backend '{host_tag}': not in the config's backends "
            f"({available}); pass --host <tag> or --server-url"
        )
    else:
        raise CfdClientError(
            "no backend selected: set 'default_backend' in the config, or pass "
            f"--host <tag> or --server-url ({path or CONFIG_BASENAME})"
        )

    if server_url_override:
        backend["server_url"] = server_url_override
    if not backend.get("server_url"):
        raise CfdClientError(f"backend '{host_tag}' has no 'server_url'")

    backend["backend"] = host_tag  # record which backend was resolved
    return backend


# --------------------------------------------------------------------------- #
# Client
# --------------------------------------------------------------------------- #


class CfdClient:
    def __init__(
        self,
        server_url: str,
        timeout: float = DEFAULT_TIMEOUT,
        verify_tls: bool = True,
        frontend_url: str | None = None,
        progress=None,
    ):
        self.base = _normalise_url(server_url)
        self.timeout = timeout
        self.verify = verify_tls
        self.frontend_url = _normalise_url(frontend_url) if frontend_url else self.base
        self.session = requests.Session()
        # Optional callback(pct) that upload/download report transfer progress
        # to; None disables it. The client just calls it - what it does with the
        # percent (e.g. write {"progress": pct} lines) is the caller's concern.
        self.progress = progress

    @classmethod
    def from_config(cls, cfg: dict, progress=None) -> "CfdClient":
        return cls(
            server_url=cfg["server_url"],
            timeout=cfg.get("timeout", DEFAULT_TIMEOUT),
            verify_tls=cfg.get("verify_tls", True),
            frontend_url=cfg.get("frontend_url"),
            progress=progress,
        )

    # -- URL helpers -------------------------------------------------------- #

    def _rw(self, name: str) -> str:
        return f"{self.base}/api/rw/{quote(name)}"

    # -- error handling ----------------------------------------------------- #

    @staticmethod
    def _server_message(resp) -> str:
        try:
            body = resp.json()
            if isinstance(body, dict):
                return str(body.get("message") or body.get("error") or body)
        except ValueError:
            pass
        return (resp.text or "").strip()[:200]

    def _raise_for_status(self, resp, context: str) -> None:
        if not resp.ok:
            raise CfdClientError(f"{context}: HTTP {resp.status_code} {self._server_message(resp)}")

    def _request(self, method: str, url: str, context: str, not_found: str | None = None, **kwargs):
        """Single choke point for every server call: it wraps the request in the
        try/except-RequestException, the optional friendly 404, and the non-2xx
        check that every operation would otherwise repeat.

        ``timeout``/``verify`` default to the client's; any other requests kwargs
        (``params``, ``headers``, ``data``, ``stream``, ...) pass through. If
        ``not_found`` is given, a 404 raises ``{context}: {not_found}`` instead
        of the generic status message. Returns the Response - call ``.json()``
        or stream it as the caller needs."""
        kwargs.setdefault("timeout", self.timeout)
        kwargs.setdefault("verify", self.verify)
        try:
            resp = self.session.request(method, url, **kwargs)
        except requests.RequestException as e:
            raise CfdClientError(f"{context}: {e}")
        if not_found is not None and resp.status_code == 404:
            raise CfdClientError(f"{context}: {not_found}")
        self._raise_for_status(resp, context)
        return resp

    # -- operations --------------------------------------------------------- #

    def upload(self, local_path, remote_name: str | None = None) -> dict:
        """Upload a file, or a directory (zipped first), as the raw request body.

        A directory is packed into a temporary .zip with its contents at the
        archive root (matching the IDA ICE case-zip layout: building.opf,
        geometry/, ... at the top level) and uploaded as ``<dir name>.zip``
        unless remote_name is given. Returns a small result dict."""
        local_path = Path(local_path)
        if local_path.is_dir():
            return self._upload_dir(local_path, remote_name)
        if not local_path.is_file():
            raise CfdClientError(f"upload: local path not found: {local_path}")
        return self._upload_file(local_path, remote_name or local_path.name)

    def _upload_file(self, local_path: Path, remote_name: str) -> dict:
        size = local_path.stat().st_size
        if size == 0:
            # The server ignores a zero-length body (content_length > 0 guard).
            raise CfdClientError(f"upload: refusing to send empty file {local_path}")
        progress = self.progress
        if progress:
            progress(0)  # initialise the bar before the transfer starts
        with local_path.open("rb") as fh:
            # data=<file object> -> requests sets Content-Length from the file
            # size and sends a non-chunked body, which the server needs.
            # _ProgressReader preserves that len() while reporting reads.
            body = _ProgressReader(fh, size, progress) if progress else fh
            self._request(
                "POST",
                self._rw(remote_name),
                "upload",
                data=body,
                headers={"Content-Type": "application/octet-stream"},
            )
        if progress:
            progress(100)  # guarantee a terminal 100 % even for a tiny body
        return {"uploaded": remote_name, "bytes": size, "source": str(local_path)}

    def _upload_dir(self, dir_path: Path, remote_name: str | None) -> dict:
        """Zip a directory (contents at the archive root) to a temp file and
        upload it. The temp zip is always removed afterwards."""
        remote_name = remote_name or f"{dir_path.name}.zip"
        files = sorted(p for p in dir_path.rglob("*") if p.is_file())
        if not files:
            raise CfdClientError(f"upload: directory has no files to zip: {dir_path}")
        tmpdir = Path(tempfile.mkdtemp(prefix="ida_cfd_"))
        zip_path = tmpdir / "upload.zip"
        try:
            with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as zf:
                for f in files:
                    # arcname relative to dir_path -> contents sit at the zip root
                    zf.write(f, f.relative_to(dir_path).as_posix())
            result = self._upload_file(zip_path, remote_name)
        finally:
            shutil.rmtree(tmpdir, ignore_errors=True)
        result.update(source=str(dir_path), zipped=True, files=len(files))
        return result

    def download(self, remote_name: str, local_dest) -> dict:
        """Download a file. If local_dest is a directory it is joined with the
        remote name. Writes to a .part temp then atomically renames."""
        progress = self.progress
        local_dest = Path(local_dest)
        if local_dest.is_dir() or str(local_dest).endswith(("/", os.sep)):
            local_dest = local_dest / remote_name
        resp = self._request(
            "GET",
            self._rw(remote_name),
            "download",
            not_found=f"not found on server: {remote_name}",
            stream=True,
        )
        # Content-Length lets us report a percentage; a chunked reply (no
        # length) simply gets no progress lines beyond the initial 0.
        total = int(resp.headers.get("Content-Length") or 0)
        if progress:
            progress(0)
        local_dest.parent.mkdir(parents=True, exist_ok=True)
        tmp = local_dest.with_name(local_dest.name + ".part")
        n = 0
        try:
            with tmp.open("wb") as fh:
                for chunk in resp.iter_content(chunk_size=CHUNK):
                    if chunk:
                        fh.write(chunk)
                        n += len(chunk)
                        if progress and total > 0:
                            progress(n * 100 / total)
            os.replace(tmp, local_dest)  # atomic on the same volume (Windows ok)
        finally:
            if tmp.exists():
                tmp.unlink()
        if progress:
            progress(100)
        return {"downloaded": remote_name, "path": str(local_dest), "bytes": n}

    def ls_fs(self, name: str | None = None):
        """List the flat upload/transfer area (/api/rw).

        Without a name: the list of file-info dicts. With a name: that single
        file's info dict (server metadata), or {} if absent. Same shape as
        ls_wd, for the transfer area instead of the working dirs."""
        tail = f"/{quote(name, safe='/')}" if name else ""
        return self._request("GET", f"{self.base}/api/rw/ls{tail}", "ls-fs").json()

    def newest(self, stem: str) -> dict:
        """Return the info dict of the newest transfer-area file whose name stem
        matches `stem` (e.g. 'file-a' -> the more recent of file-a.zip and
        file-a.downstage), or {} if none match. One request instead of listing
        and comparing mtimes client-side."""
        return self._request(
            "GET", f"{self.base}/api/rw/newest/{quote(stem, safe='')}", "newest"
        ).json()

    def ls_path(self, path: str):
        """List any path within the server's CFD_HOME (/api/ls).

        A directory returns its {"dirs": [...], "files": [...]} contents; a
        single file returns the same shape with that file as a one-element
        "files" list. Unlike ls_fs, an absent path 404s (there is no "list all"
        form - the general browser always addresses a specific path)."""
        return self._request(
            "GET",
            f"{self.base}/api/ls/{quote(path, safe='/')}",
            "ls",
            not_found=f"path not found under CFD_HOME: {path}",
        ).json()

    def ls_wd(self, case_name: str | None = None):
        """List working directories under the server's CFD_HOME.

        Without a name: the list of cases (dir entries). With a name: the
        contents ({"dirs": [...], "files": [...]}) of that case directory, or
        an empty result if the case is not found. This is distinct from ls(),
        which lists the flat upload/transfer area (/api/rw)."""
        tail = f"/{quote(case_name, safe='/')}" if case_name else ""
        return self._request("GET", f"{self.base}/api/wd/ls{tail}", "ls-wd").json()

    def metadata(self, case_path: str) -> dict:
        """Full metadata for a working-dir case: the IDA ICE origin fields plus
        a freshly refreshed ``case_info`` (latest time, ncells, ...) — one call
        for everything about a case (GET /api/metadata)."""
        return self._request(
            "GET", f"{self.base}/api/metadata/{quote(case_path, safe='/')}", "metadata"
        ).json()

    def exists(self, remote_name: str) -> bool:
        """True if a transfer-area file exists (ls_fs returns {} for an absent
        name)."""
        return bool(self.ls_fs(remote_name))

    def exists_wd(self, remote_name: str) -> bool:
        """True if a transfer-area file exists (ls_fs returns {} for an absent
        name)."""
        return bool(self.ls_path(remote_name))

    def delete(self, remote_name: str) -> dict:
        return self._request(
            "DELETE",
            self._rw(remote_name),
            "delete",
            not_found=f"not found on server: {remote_name}",
        ).json()

    def cleanup(self) -> dict:
        return self._request("GET", f"{self.base}/api/rw/cleanup_folders", "cleanup").json()

    def upstage(self, remote_name: str, wd: str | None = None) -> dict:
        """Stage an already-uploaded file into a backend working directory.

        Calls ``/api/upstage/<wd>`` with query params ``wd`` and ``url``, where
        url points at the uploaded file (``<base>/api/rw/<remote_name>``). The
        backend HEAD-checks the url, creates wd under CFD_HOME, and starts a
        background job that fetches/unpacks the file into it.

        POST is used (not GET) because the backend returns JSON unconditionally
        for POST, whereas a GET would 302-redirect to the cockpit page unless an
        Accept: application/json header is honoured. The params still travel in
        the query string, which the backend reads regardless of method.

        Note: url is built from this client's base URL. If the file server and
        the backend are split across hosts, the backend must be able to reach
        that url; add a separate file-server URL to the config if needed."""
        wd = wd or Path(remote_name).stem
        if not wd:
            raise CfdClientError("upstage: --wd is required")
        file_url = self._rw(remote_name)
        endpoint = f"{self.base}/api/upstage/{quote(wd, safe='/')}"
        return self._request(
            "POST",
            endpoint,
            "upstage",
            not_found=(
                "backend could not reach the uploaded file or resolve the "
                f"working dir (url={file_url}, wd={wd})"
            ),
            params={"wd": wd, "url": file_url},
            headers={"Accept": "application/json"},
        ).json()

    def downstage(self, case_path: str) -> dict:
        """Pack a processed case on the server and publish it for download.

        The reverse of upstage: POST /api/downstage/<case_path>, which
        reconstructs + zips the case at case_path and uploads it to the server's
        file store as ``<name>.downstage`` (the suffix marks a processed case).
        Runs synchronously; returns the name/url to fetch - then call download()
        with that name. POST is used so the reply is always JSON (a GET can
        302-redirect to the cockpit page)."""
        if not case_path:
            raise CfdClientError("downstage: case_path is required")
        endpoint = f"{self.base}/api/downstage/{quote(case_path, safe='/')}"
        return self._request(
            "POST",
            endpoint,
            "downstage",
            not_found=f"case not found on server: {case_path}",
            headers={"Accept": "application/json"},
        ).json()


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #


def _emit(result, ok: bool = True) -> int:
    """Print one JSON object to stdout and return the process exit code.

    stdout carries ONLY this object (single line); human/diagnostic text goes
    to stderr, so a subprocess caller can parse stdout as JSON unconditionally."""
    payload = (
        {"ok": ok, "exit": 0, "result": result}
        if ok
        else {"ok": False, "exit": 1, "error": str(result)}
    )
    json.dump(payload, sys.stdout)
    sys.stdout.write("\n")
    return 0 if ok else 1


class _JsonArgumentParser(argparse.ArgumentParser):
    """ArgumentParser that reports usage errors as a JSON object on stdout
    (and exits 1), instead of argparse's default human text on stderr - so the
    stdout-is-always-JSON contract holds even for bad arguments. (The one
    exception is -h/--help, which still prints human help to stdout.)"""

    def error(self, message):  # called by argparse on invalid/missing arguments
        self.print_usage(sys.stderr)  # keep the usage hint on stderr for humans
        _emit(f"usage error: {message}", ok=False)
        raise SystemExit(1)


def build_parser() -> argparse.ArgumentParser:
    # Subparsers inherit this class (argparse default parser_class = type(self)),
    # so their argument errors emit JSON too.
    p = _JsonArgumentParser(
        prog="ida_cfd_client.py",
        description="Upload/download CFD case data to/from the cfd-backend.",
    )
    p.add_argument("--config", help=f"path to config JSON (default: {CONFIG_BASENAME})")
    p.add_argument(
        "--host",
        help="backend tag to use from the config's 'backends' "
        "(default: the config's 'default_backend')",
    )
    p.add_argument("--server-url", help="override the resolved backend's server_url")
    p.add_argument(
        "--progress",
        action="store_true",
        help='emit throttled {"progress": <pct>} JSON lines during upload/'
        "download, before the final result line (no-op for other commands)",
    )
    sub = p.add_subparsers(dest="command", required=True)

    up = sub.add_parser("upload", help="upload a local file, or a directory (zipped)")
    up.add_argument("local_path", help="a file, or a directory to zip and upload")
    up.add_argument("--name", help="remote name (default: the file name, or <dir>.zip)")

    dn = sub.add_parser("download", help="download a remote file")
    dn.add_argument("remote_name")
    dn.add_argument("local_dest", help="destination file or directory")

    lsp = sub.add_parser(
        "ls", help="list any path under CFD_HOME /api/ls (a dir's contents, or one file)"
    )
    lsp.add_argument("path", help="a path relative to CFD_HOME (a directory or a file)")

    lfs = sub.add_parser(
        "ls-fs", help="list the transfer area /api/rw (or one file's info, given a name)"
    )
    lfs.add_argument(
        "name", nargs="?", default=None, help="a remote file name; omit to list all files"
    )

    lw = sub.add_parser(
        "ls-wd", help="list working dirs under CFD_HOME (cases, or one case's contents)"
    )
    lw.add_argument(
        "case_name", nargs="?", default=None, help="a case name; omit to list all cases"
    )

    md = sub.add_parser(
        "metadata", help="full case metadata (IDA fields + refreshed case_info: time, ncells)"
    )
    md.add_argument("case_path", help="the case working dir under CFD_HOME")

    nw = sub.add_parser(
        "newest",
        help="newest transfer-area file sharing a stem (e.g. file-a -> file-a.zip/.downstage)",
    )
    nw.add_argument("stem", help="the filename stem to match (name without its extension)")

    ex = sub.add_parser("exists", help="check whether a remote file exists")
    ex.add_argument("remote_name")

    rm = sub.add_parser("rm", help="delete a remote file")
    rm.add_argument("remote_name")

    us = sub.add_parser("upstage", help="stage an uploaded file into a backend working dir")
    us.add_argument("remote_name", help="name of the already-uploaded file (its URL is inferred)")
    us.add_argument(
        "--wd",
        required=False,
        default=None,
        help="working directory (name or path-like string) under CFD_HOME"
        "defaults to remote_name stem",
    )

    ds = sub.add_parser(
        "downstage", help="pack a processed case server-side and publish it for download"
    )
    ds.add_argument(
        "case_path", help="the case working dir on the server (name or path-like string)"
    )

    ur = sub.add_parser("url", help="print the cfd-frontend URL")
    ur.add_argument("--open", action="store_true", help="open it in a browser")

    sub.add_parser("config", help="dump json config")

    sub.add_parser("cleanup", help="ask the server to purge old files")
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        cfg = load_config(args.config, args.server_url, args.host)
        progress = _make_progress_emitter() if args.progress else None
        client = CfdClient.from_config(cfg, progress=progress)

        if args.command == "config":
            return _emit(cfg)
        if args.command == "upload":
            return _emit(client.upload(args.local_path, args.name))
        if args.command == "download":
            return _emit(client.download(args.remote_name, args.local_dest))
        if args.command == "ls":
            return _emit(client.ls_path(args.path))
        if args.command == "ls-fs":
            return _emit(client.ls_fs(args.name))
        if args.command == "newest":
            return _emit(client.newest(args.stem))
        if args.command == "ls-wd":
            return _emit(client.ls_wd(args.case_name))
        if args.command == "metadata":
            return _emit(client.metadata(args.case_path))
        if args.command == "exists":
            return _emit({"exists": client.exists(args.remote_name)})
        if args.command == "rm":
            return _emit(client.delete(args.remote_name))
        if args.command == "upstage":
            return _emit(client.upstage(args.remote_name, args.wd))
        if args.command == "downstage":
            return _emit(client.downstage(args.case_path))
        if args.command == "cleanup":
            return _emit(client.cleanup())
        if args.command == "url":
            if args.open:
                import webbrowser

                webbrowser.open(client.frontend_url)
            return _emit({"frontend_url": client.frontend_url})
    except CfdClientError as e:
        return _emit(e, ok=False)
    except Exception as e:  # last-resort guard so the caller always gets JSON
        return _emit(f"unexpected error: {e}", ok=False)
    return _emit("no command", ok=False)


if __name__ == "__main__":
    sys.exit(main())
