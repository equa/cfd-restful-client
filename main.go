// Command cfd-client moves CFD case data to/from the EQUA cfd-backend. IDA ICE
// calls it as a subprocess.
//
// Output contract: stdout carries ONLY a single JSON object (one line); with
// --progress on upload/download, throttled {"progress": <pct>} lines precede it.
// The exit code is 0 on success, 1 on failure (including bad arguments). All
// human / diagnostic text goes to stderr. So a subprocess caller (IDA ICE) can
// parse stdout as JSON unconditionally and branch on the code.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// -- output contract -------------------------------------------------------- //

type okEnvelope struct {
	OK     bool `json:"ok"`
	Exit   int  `json:"exit"`
	Result any  `json:"result"`
}

type errEnvelope struct {
	OK    bool   `json:"ok"`
	Exit  int    `json:"exit"`
	Error string `json:"error"`
}

func emit(payload any) {
	// SetEscapeHTML(false) so <, > and & stay literal (matches the Python
	// output); Encode writes a single compact line plus a trailing newline.
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		os.Stdout.Write([]byte(`{"ok":false,"exit":1,"error":"json marshal error"}` + "\n"))
	}
}

func emitOK(result any) int {
	emit(okEnvelope{OK: true, Exit: 0, Result: result})
	return 0
}

func emitErr(msg string) int {
	emit(errEnvelope{OK: false, Exit: 1, Error: msg})
	return 1
}

// emitStatus reports a well-behaved outcome that is NOT a tool failure: exit 0
// (the call ran fine), with an explicit ok flag -- false when the requested
// artifact isn't there (e.g. status "missing"), so IDA can distinguish "nothing
// to fetch" from a crash.
func emitStatus(ok bool, result any) int {
	emit(okEnvelope{OK: ok, Exit: 0, Result: result})
	return 0
}

// makeProgressEmitter returns a callback that writes throttled {"progress": pct}
// NDJSON lines to stdout: the first call, then only when the integer percent has
// advanced by 5 or reached 100 -- never the same value twice.
func makeProgressEmitter() progressFn {
	last := -1
	return func(pct float64) {
		p := int(pct)
		if p < 0 {
			p = 0
		} else if p > 100 {
			p = 100
		}
		if last < 0 || p-last >= 5 || (p == 100 && last != 100) {
			last = p
			fmt.Fprintf(os.Stdout, "{\"progress\": %d}\n", p)
		}
	}
}

// -- argument parsing ------------------------------------------------------- //

const usage = `cfd-client -- upload/download CFD case data to/from the cfd-backend.

usage: cfd-client [--config PATH] [--host TAG] [--server-url URL] [--progress] <command> ...

  upload    <local_path> [--name REMOTE]   a file, or a directory (zipped)
  download  <remote_name> <local_dest>
  ls        <path>          any path under CFD_HOME (/api/ls): dir contents, or a file
  ls-fs     [name]          transfer area (/api/rw): all files, or one file's info
  ls-wd     [case]          working dirs: all cases, or one case's contents
  metadata  <case>          full case metadata (IDA fields + case_info)
  newest    <stem>          newest transfer-area file sharing a name stem
  exists    <remote_name>
  rm        <remote_name>
  upstage   <remote_name> [--wd NAME]   stage an uploaded file server-side
  ensure    <case-id> [--force]         make sure a case is staged (resume); backend
                                        stages from the file server if needed
  save      <case-id> [--since MTIME]   newest downloadable archive of a case
                                        (publishes only if newer work exists)
  downstage <case_path>                 pack a processed case, publish for download
  url       [--open]        print (or open) the cfd-frontend URL
  config                    dump the resolved backend config
  cleanup                   ask the server to purge old files
`

// parseCmd splits command args into positionals, known bool flags, and known
// string flags. An unknown --option is an error.
func parseCmd(args []string, boolFlags map[string]*bool, strFlags map[string]*string) ([]string, error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			key := a[2:]
			val := ""
			hasEq := false
			if eq := strings.IndexByte(key, '='); eq >= 0 {
				val, hasEq = key[eq+1:], true
				key = key[:eq]
			}
			if p, ok := boolFlags[key]; ok {
				if hasEq {
					return nil, fmt.Errorf("option --%s takes no value", key)
				}
				*p = true
				continue
			}
			if p, ok := strFlags[key]; ok {
				if !hasEq {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("option --%s requires a value", key)
					}
					i++
					val = args[i]
				}
				*p = val
				continue
			}
			return nil, fmt.Errorf("unknown option --%s", key)
		}
		pos = append(pos, a)
	}
	return pos, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// Global flags come before the command (like the Python argparse layout).
	var config, host, serverURL string
	progress := false
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "-h" || a == "--help" {
			fmt.Fprint(os.Stdout, usage)
			return 0
		}
		if !strings.HasPrefix(a, "--") {
			break
		}
		key := a[2:]
		val := ""
		hasEq := false
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			val, hasEq = key[eq+1:], true
			key = key[:eq]
		}
		takesValue := key == "config" || key == "host" || key == "server-url"
		if takesValue && !hasEq {
			if i+1 >= len(args) {
				return emitErr(fmt.Sprintf("usage error: option --%s requires a value", key))
			}
			i++
			val = args[i]
		}
		switch key {
		case "config":
			config = val
		case "host":
			host = val
		case "server-url":
			serverURL = val
		case "progress":
			if hasEq {
				return emitErr("usage error: option --progress takes no value")
			}
			progress = true
		default:
			return emitErr(fmt.Sprintf("usage error: unknown option --%s", key))
		}
		i++
	}

	if i >= len(args) {
		return emitErr("usage error: a command is required")
	}
	command := args[i]
	cmdArgs := args[i+1:]

	backend, err := loadConfig(config, serverURL, host)
	if err != nil {
		return emitErr(err.Error())
	}

	// `config` just dumps the resolved backend -- no client needed.
	if command == "config" {
		if len(cmdArgs) != 0 {
			return emitErr("usage error: config takes no arguments")
		}
		return emitOK(backend)
	}

	var prog progressFn
	if progress {
		prog = makeProgressEmitter()
	}
	client, err := newClientFromBackend(backend, prog)
	if err != nil {
		return emitErr(err.Error())
	}

	switch command {
	case "upload":
		var name string
		pos, err := parseCmd(cmdArgs, nil, map[string]*string{"name": &name})
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: upload requires <local_path>")
		}
		return dispatch(client.upload(pos[0], name))

	case "download":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 2 {
			return emitErr("usage error: download requires <remote_name> <local_dest>")
		}
		result, err := client.download(pos[0], pos[1])
		if err != nil {
			return emitErr(err.Error())
		}
		// A missing file is not a failure (same as save): exit 0, ok:false.
		if m, ok := result.(map[string]any); ok && m["status"] == "missing" {
			return emitStatus(false, result)
		}
		return dispatch(result, nil)

	case "ls":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: ls requires <path>")
		}
		return dispatch(client.lsPath(pos[0]))

	case "ls-fs":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) > 1 {
			return emitErr("usage error: ls-fs takes an optional [name]")
		}
		return dispatch(client.lsFs(first(pos)))

	case "ls-wd":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) > 1 {
			return emitErr("usage error: ls-wd takes an optional [case]")
		}
		return dispatch(client.lsWd(first(pos)))

	case "metadata":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: metadata requires <case>")
		}
		return dispatch(client.metadata(pos[0]))

	case "newest":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: newest requires <stem>")
		}
		return dispatch(client.newest(pos[0]))

	case "exists":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: exists requires <remote_name>")
		}
		ok, err := client.exists(pos[0])
		if err != nil {
			return emitErr(err.Error())
		}
		return emitOK(map[string]any{"exists": ok})

	case "rm":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: rm requires <remote_name>")
		}
		return dispatch(client.delete(pos[0]))

	case "upstage":
		var wd string
		pos, err := parseCmd(cmdArgs, nil, map[string]*string{"wd": &wd})
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: upstage requires <remote_name>")
		}
		return dispatch(client.upstage(pos[0], wd))

	case "ensure":
		force := false
		pos, err := parseCmd(cmdArgs, map[string]*bool{"force": &force}, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: ensure requires <case-id>")
		}
		return dispatch(client.ensure(pos[0], force))

	case "save":
		var since string
		pos, err := parseCmd(cmdArgs, nil, map[string]*string{"since": &since})
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: save requires <case-id>")
		}
		var s float64
		if since != "" {
			if s, err = strconv.ParseFloat(since, 64); err != nil {
				return emitErr("usage error: --since must be a number (mtime)")
			}
		}
		result, err := client.save(pos[0], s)
		if err != nil {
			return emitErr(err.Error())
		}
		// "missing" (no case and no archive) is a well-behaved outcome, not a
		// failure: exit 0 so IDA can tell it apart from a crash, with ok:false to
		// signal that no archive was produced.
		if m, ok := result.(map[string]any); ok && m["status"] == "missing" {
			return emitStatus(false, result)
		}
		return dispatch(result, nil)

	case "downstage":
		pos, err := parseCmd(cmdArgs, nil, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 1 {
			return emitErr("usage error: downstage requires <case_path>")
		}
		return dispatch(client.downstage(pos[0]))

	case "cleanup":
		if len(cmdArgs) != 0 {
			return emitErr("usage error: cleanup takes no arguments")
		}
		return dispatch(client.cleanup())

	case "url":
		open := false
		pos, err := parseCmd(cmdArgs, map[string]*bool{"open": &open}, nil)
		if err != nil {
			return emitErr("usage error: " + err.Error())
		}
		if len(pos) != 0 {
			return emitErr("usage error: url takes no positional arguments")
		}
		if open {
			if err := openBrowser(client.frontendURL); err != nil {
				fmt.Fprintln(os.Stderr, "could not open browser:", err)
			}
		}
		return emitOK(map[string]any{"frontend_url": client.frontendURL})

	default:
		return emitErr("usage error: unknown command " + strconv.Quote(command))
	}
}

// dispatch turns an (result, error) pair into the JSON envelope + exit code.
func dispatch(result any, err error) int {
	if err != nil {
		return emitErr(err.Error())
	}
	return emitOK(result)
}

func first(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
