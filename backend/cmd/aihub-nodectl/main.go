// Command aihub-nodectl is the privileged helper that lets the AIHub backend
// manage OpenFlux exit channels without root of its own. It only allows
// apply, remove, list and status of /opt/openflux-node/node-install.sh.
//
//	aihub-nodectl apply              config (key=value lines) on stdin
//	aihub-nodectl remove CHANNEL
//	aihub-nodectl status CHANNEL
//	aihub-nodectl list
//	aihub-nodectl serve [-socket PATH] [-group GROUP]
//
// CLI mode is meant for a sudoers rule that allows exactly this binary;
// serve mode listens on a unix socket (0660, the given group) for a
// containerised backend. Output is the script's JSON on stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/ItzChelove4ik/AIHub/backend/internal/nodectl"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	runner := &nodectl.Runner{Script: nodectl.DefaultScript}
	if s := os.Getenv("AIHUB_NODECTL_SCRIPT"); s != "" && os.Geteuid() != 0 {
		// Only for tests as a normal user: root always runs the fixed path.
		runner.Script = s
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	op := os.Args[1]
	switch op {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		socket := fs.String("socket", "/run/aihub-nodectl/nodectl.sock", "unix socket path")
		group := fs.String("group", "aihub", "group allowed to use the socket")
		_ = fs.Parse(os.Args[2:])
		if err := serve(ctx, runner, *socket, *group); err != nil {
			log.Fatal(err)
		}
		return
	case "apply", "list", "remove", "status":
	default:
		usage()
	}
	req := nodectl.Request{Op: op}
	switch op {
	case "apply":
		cfg, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
		if err != nil {
			fail(err)
		}
		req.Config = string(cfg)
	case "remove", "status":
		if len(os.Args) != 3 {
			usage()
		}
		req.Channel = os.Args[2]
	}
	out, err := runner.Run(ctx, req)
	if len(out) > 0 {
		fmt.Println(string(out))
	}
	if err != nil {
		if len(out) == 0 {
			fail(err)
		}
		os.Exit(1)
	}
}

func serve(ctx context.Context, runner *nodectl.Runner, socket, group string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return err
	}
	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	gid := -1
	if g, err := user.LookupGroup(group); err == nil {
		gid, _ = strconv.Atoi(g.Gid)
	} else {
		log.Printf("group %q not found, socket stays root-only", group)
	}
	if gid >= 0 {
		_ = os.Chown(filepath.Dir(socket), 0, gid)
		_ = os.Chown(socket, 0, gid)
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		return err
	}
	log.Printf("aihub-nodectl listening on %s", socket)
	return nodectl.Serve(ctx, l, runner, log.Printf)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: aihub-nodectl apply|list|remove CHANNEL|status CHANNEL|serve [-socket PATH] [-group GROUP]")
	os.Exit(2)
}

func fail(err error) {
	fmt.Printf("{\"ok\":false,\"step\":\"nodectl\",\"error\":%q}\n", err.Error())
	os.Exit(1)
}
