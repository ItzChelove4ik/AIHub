// Package nodectl is the privileged helper between the backend and
// OpenFlux's node-install.sh. It allows exactly four operations (apply,
// remove, list, status), validates every config line, and hands the config
// to the script in a 0600 temp file, as the script requires, so no secret
// ever appears in a command line.
//
// The helper runs as root, either as a CLI the backend calls through a
// sudoers rule (aihub-nodectl apply|remove|list|status) or as a small
// daemon on a unix socket that a containerised backend can reach without
// getting the Docker socket or sudo.
package nodectl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultScript is AIHub's own pinned copy of node-install.sh. Not
	// /opt/openflux-node/node-install.sh: the script deletes that directory
	// when the last channel goes, and its updater rewrites the file there.
	DefaultScript = "/usr/local/lib/aihub/node-install.sh"
	maxConfig     = 16 << 10
	maxOutput     = 256 << 10
)

// Request is one operation.
type Request struct {
	Op      string `json:"op"`                // apply | remove | list | status
	Config  string `json:"config,omitempty"`  // apply: key=value lines
	Channel string `json:"channel,omitempty"` // remove, status
}

// Response is what the socket server answers.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"` // the script's JSON
	Error  string          `json:"error,omitempty"`
}

// ScriptError is a failure the script reported ({"ok":false,...}).
type ScriptError struct {
	Step    string `json:"step"`
	Message string `json:"error"`
}

func (e *ScriptError) Error() string {
	if e.Step != "" {
		return e.Step + ": " + e.Message
	}
	return e.Message
}

var (
	channelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	keyRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	yandexRe  = regexp.MustCompile(`^https://(docs|disk)\.yandex\.(ru|com|by|kz|uz)/edit/d/[A-Za-z0-9_-]{16,200}$`)
	mailruRe  = regexp.MustCompile(`^https://cloud\.mail\.ru/public/[A-Za-z0-9_-]{2,64}/[A-Za-z0-9_-]{2,128}$`)
	roomsRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// ValidChannel reports whether name is a valid channel name.
func ValidChannel(name string) bool { return channelRe.MatchString(name) }

// ValidateConfig checks an apply config the way node-install.sh does,
// stricter: every key is known, appears once, and channel, key and port
// are present.
func ValidateConfig(cfg string) error {
	if len(cfg) > maxConfig {
		return errors.New("config too large")
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(cfg, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("config line without '='")
		}
		if seen[k] {
			return fmt.Errorf("config key %q repeated", k)
		}
		seen[k] = true
		var valid bool
		switch k {
		case "channel":
			valid = channelRe.MatchString(v)
		case "key":
			valid = keyRe.MatchString(v)
		case "port":
			p, err := strconv.Atoi(v)
			valid = err == nil && p >= 1024 && p <= 65535 && strconv.Itoa(p) == v
		case "vyandex":
			valid = yandexRe.MatchString(v)
		case "mailru":
			valid = mailruRe.MatchString(v)
		case "cupsonline":
			valid = len(v) >= 8 && len(v) <= 4096 && roomsRe.MatchString(v)
		case "autoupdate":
			valid = v == "yes" || v == "no"
		default:
			return fmt.Errorf("config key %q not allowed", k)
		}
		if !valid {
			// The value may be a secret: name the key only.
			return fmt.Errorf("config key %q has an invalid value", k)
		}
	}
	for _, need := range []string{"channel", "key", "port"} {
		if !seen[need] {
			return fmt.Errorf("config needs %s", need)
		}
	}
	return nil
}

// Runner runs node-install.sh.
type Runner struct {
	Script  string // path of node-install.sh
	Shell   string // /bin/sh
	TempDir string // where config files go; "" = os.TempDir()
	Timeout time.Duration
}

func (r *Runner) defaults() {
	if r.Script == "" {
		r.Script = DefaultScript
	}
	if r.Shell == "" {
		r.Shell = "/bin/sh"
	}
	if r.Timeout == 0 {
		r.Timeout = 5 * time.Minute // apply downloads the core
	}
}

// writeTemp writes content to a fresh 0600 file and returns its path.
func (r *Runner) writeTemp(content string) (path string, err error) {
	f, err := os.CreateTemp(r.TempDir, "aihub-nodectl-*.conf")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(0o600); err != nil && !isWindows() {
		return "", err
	}
	if _, err = f.WriteString(content); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// Run performs one operation and returns the script's JSON output.
func (r *Runner) Run(ctx context.Context, req Request) (json.RawMessage, error) {
	r.defaults()
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var args []string
	var stdin io.Reader
	switch req.Op {
	case "apply":
		if err := ValidateConfig(req.Config); err != nil {
			return nil, err
		}
		path, err := r.writeTemp(req.Config)
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.Remove(path) }() // the script deletes it after reading; this covers failures
		args = []string{"apply", path}
	case "remove":
		if !channelRe.MatchString(req.Channel) {
			return nil, errors.New("invalid channel name")
		}
		path, err := r.writeTemp("channel=" + req.Channel + "\n")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.Remove(path) }()
		args = []string{"remove", path}
	case "status":
		if !channelRe.MatchString(req.Channel) {
			return nil, errors.New("invalid channel name")
		}
		args = []string{"status"}
		stdin = strings.NewReader("channel=" + req.Channel + "\n")
	case "list":
		args = []string{"list"}
	default:
		return nil, fmt.Errorf("operation %q not allowed", req.Op)
	}
	cmd := exec.CommandContext(ctx, r.Shell, append([]string{r.Script}, args...)...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	cmd.Stdin = stdin
	var stdout limitedBuffer
	stdout.max = maxOutput
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	runErr := cmd.Run()
	out := bytes.TrimSpace(stdout.Bytes())
	// The script prints one JSON object; take the last line in case
	// anything else slipped out.
	if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	var probe struct {
		OK *bool `json:"ok"`
		ScriptError
	}
	if len(out) == 0 || json.Unmarshal(out, &probe) != nil || probe.OK == nil {
		if runErr != nil {
			return nil, fmt.Errorf("node-install.sh %s failed: %w", req.Op, runErr)
		}
		return nil, fmt.Errorf("node-install.sh %s printed no JSON", req.Op)
	}
	if !*probe.OK {
		return json.RawMessage(out), &ScriptError{Step: probe.Step, Message: probe.Message}
	}
	return json.RawMessage(out), nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// Serve answers one request per connection on l until ctx ends.
func Serve(ctx context.Context, l net.Listener, r *Runner, logf func(format string, args ...any)) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go handleConn(ctx, conn, r, logf)
	}
}

func handleConn(ctx context.Context, conn net.Conn, r *Runner, logf func(string, ...any)) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReaderSize(io.LimitReader(conn, maxConfig+1024), maxConfig+1024).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req Request
	resp := Response{}
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = "bad request"
	} else {
		result, err := r.Run(ctx, req)
		resp.Result = result
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.OK = true
		}
		// Log the operation and channel only: never the config.
		logf("op=%s channel=%s ok=%v", req.Op, req.Channel, resp.OK)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = json.NewEncoder(conn).Encode(resp)
}

func isWindows() bool { return os.PathSeparator == '\\' }
