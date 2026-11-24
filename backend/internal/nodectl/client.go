package nodectl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
)

// Client is how the backend reaches the helper.
type Client interface {
	Run(ctx context.Context, req Request) (json.RawMessage, error)
}

// SocketClient talks to `aihub-nodectl serve` over a unix socket.
type SocketClient struct{ Path string }

func (c *SocketClient) Run(ctx context.Context, req Request) (json.RawMessage, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, fmt.Errorf("aihub-nodectl недоступен: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	var resp Response
	if err := json.NewDecoder(io.LimitReader(conn, maxOutput+4096)).Decode(&resp); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("aihub-nodectl: %w", err)
	}
	if !resp.OK {
		return resp.Result, scriptErrorFrom(resp.Result, resp.Error)
	}
	return resp.Result, nil
}

// ExecClient runs the helper as a command, e.g. "sudo -n
// /usr/local/bin/aihub-nodectl". The config goes on stdin.
type ExecClient struct{ Command []string }

func (c *ExecClient) Run(ctx context.Context, req Request) (json.RawMessage, error) {
	if len(c.Command) == 0 {
		return nil, errors.New("NODECTL_COMMAND is empty")
	}
	args := append(append([]string{}, c.Command[1:]...), req.Op)
	if req.Op == "remove" || req.Op == "status" {
		args = append(args, req.Channel)
	}
	cmd := exec.CommandContext(ctx, c.Command[0], args...)
	cmd.Stdin = strings.NewReader(req.Config)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	runErr := cmd.Run()
	raw := bytes.TrimSpace(out.Bytes())
	if len(raw) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("aihub-nodectl %s: %w", req.Op, runErr)
		}
		return nil, fmt.Errorf("aihub-nodectl %s printed nothing", req.Op)
	}
	var probe struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.OK == nil {
		return nil, fmt.Errorf("aihub-nodectl %s: unexpected output", req.Op)
	}
	if !*probe.OK {
		return raw, scriptErrorFrom(raw, "")
	}
	return raw, nil
}

func scriptErrorFrom(raw json.RawMessage, fallback string) error {
	var se ScriptError
	if len(raw) > 0 && json.Unmarshal(raw, &se) == nil && se.Message != "" {
		return &se
	}
	if fallback == "" {
		fallback = "aihub-nodectl failed"
	}
	return errors.New(fallback)
}
