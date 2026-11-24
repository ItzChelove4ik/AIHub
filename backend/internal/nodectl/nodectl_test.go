package nodectl

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeScript stands in for node-install.sh: it records its arguments, the
// mode and content of the config file, and answers like the real script.
const fakeScript = `#!/bin/sh
# The helper runs the script with a clean environment: log next to it.
log="$(dirname "$0")/log"
echo "args: $*" >> "$log"
case "$1" in
  apply)
    echo "mode: $(stat -c %a "$2")" >> "$log"
    echo "config:" >> "$log"; cat "$2" >> "$log"
    rm -f "$2"
    if grep -q '^port=20001$' "$log"; then printf '{"ok":false,"step":"apply","error":"порт 20001 занят"}\n'; exit 1; fi
    printf '{"ok":true,"channel":"of-test","port":20002}\n' ;;
  remove) cat "$2" >> "$log"; rm -f "$2"; printf '{"ok":true,"channel":"of-test"}\n' ;;
  status) cat >> "$log"; printf '{"ok":true,"channel":"of-test","state":"active"}\n' ;;
  list) printf '{"ok":true,"core":"node-v1","autoupdate":true,"channels":[{"channel":"of-test","state":"active","port":20002}]}\n' ;;
  *) printf '{"ok":false,"step":"usage","error":"usage"}\n'; exit 1 ;;
esac
`

func newRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell and file modes")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "node-install.sh")
	if err := os.WriteFile(script, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "log")
	return &Runner{Script: script, Shell: "/bin/sh", TempDir: dir}, logPath
}

const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateConfig(t *testing.T) {
	good := "channel=of-abc123\nkey=" + key + "\nport=20001\nvyandex=https://docs.yandex.ru/edit/d/AAAAAAAAAAAAAAAAAAAA\ncupsonline=abcdefgh12\nautoupdate=yes\n"
	if err := ValidateConfig(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"unknown key":  good + "evil=1\n",
		"repeated key": good + "port=20002\n",
		"bad key":      "channel=of-a\nkey=xyz\nport=20001\n",
		"bad port":     "channel=of-a\nkey=" + key + "\nport=80\n",
		"bad channel":  "channel=../etc\nkey=" + key + "\nport=20001\n",
		"bad doc":      "channel=of-a\nkey=" + key + "\nport=20001\nvyandex=https://evil.example/edit/d/AAAAAAAAAAAAAAAAAAAA\n",
		"no key":       "channel=of-a\nport=20001\n",
		"injection":    "channel=of-a\nkey=" + key + "\nport=20001\nmailru=https://cloud.mail.ru/public/ab/cd$(reboot)\n",
	}
	for name, cfg := range bad {
		err := ValidateConfig(cfg)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("%s: error leaks the key", name)
		}
	}
}

func TestRunApplyUsesPrivateTempFile(t *testing.T) {
	r, logPath := newRunner(t)
	cfg := "channel=of-test\nkey=" + key + "\nport=20002\nautoupdate=yes\n"
	out, err := r.Run(context.Background(), Request{Op: "apply", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"port":20002`) {
		t.Fatalf("out: %s", out)
	}
	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "mode: 600") {
		t.Fatalf("config file must be 0600:\n%s", log)
	}
	if !strings.Contains(string(log), "key="+key) {
		t.Fatalf("script did not get the config:\n%s", log)
	}
	for _, line := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(line, "args:") && strings.Contains(line, key) {
			t.Fatalf("secret in arguments: %s", line)
		}
	}
	left, _ := filepath.Glob(filepath.Join(r.TempDir, "aihub-nodectl-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestRunScriptErrorAndOps(t *testing.T) {
	r, _ := newRunner(t)
	_, err := r.Run(context.Background(), Request{Op: "apply", Config: "channel=of-test\nkey=" + key + "\nport=20001\n"})
	var se *ScriptError
	if !errors.As(err, &se) || !strings.Contains(se.Message, "занят") {
		t.Fatalf("expected script error, got %v", err)
	}
	if _, err := r.Run(context.Background(), Request{Op: "uninstall"}); err == nil {
		t.Fatal("uninstall must not be allowed")
	}
	if _, err := r.Run(context.Background(), Request{Op: "remove", Channel: "x; rm -rf /"}); err == nil {
		t.Fatal("bad channel accepted")
	}
	for _, req := range []Request{{Op: "list"}, {Op: "status", Channel: "of-test"}, {Op: "remove", Channel: "of-test"}} {
		out, err := r.Run(context.Background(), req)
		if err != nil || !json.Valid(out) {
			t.Fatalf("%s: %s %v", req.Op, out, err)
		}
	}
}

func TestSocketServerAndClient(t *testing.T) {
	r, _ := newRunner(t)
	sock := filepath.Join(r.TempDir, "n.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, r, func(string, ...any) {})
	c := &SocketClient{Path: sock}
	out, err := c.Run(ctx, Request{Op: "list"})
	if err != nil || !strings.Contains(string(out), "of-test") {
		t.Fatalf("list: %s %v", out, err)
	}
	_, err = c.Run(ctx, Request{Op: "apply", Config: "channel=of-test\nkey=" + key + "\nport=20001\n"})
	var se *ScriptError
	if !errors.As(err, &se) {
		t.Fatalf("script error over socket: %v", err)
	}
	if _, err := c.Run(ctx, Request{Op: "uninstall"}); err == nil {
		t.Fatal("uninstall over socket allowed")
	}
}
