// streamclient claims a streaming ToolCall via the spicebox gateway, optionally
// pipes stdin into the tool, and writes stdout/stderr/exit-info to disk.
//
// Usage:
//
//	streamclient \
//	  --gateway localhost:8443 \
//	  --namespace default \
//	  --toolcall echo-stream \
//	  --token <raw gateway stream token; obtained out-of-band from the runner — it is never persisted in the ToolCall> \
//	  --out /tmp/echo-stream \
//	  [--stdin "bytes to send"]
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// config holds streamclient's flag-derived configuration.
type config struct {
	gatewayAddr string
	ns          string
	name        string
	token       string
	outDir      string
	stdinStr    string
	stdinFile   string
	timeout     time.Duration
}

func main() {
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "streamclient",
		Short:        "Claim a streaming ToolCall via the spicebox gateway and write stdout/stderr/exit to disk.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cfg)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cfg.gatewayAddr, "gateway", "localhost:8443", "gateway gRPC address")
	fs.StringVar(&cfg.ns, "namespace", "default", "ToolCall namespace")
	fs.StringVar(&cfg.name, "toolcall", "", "ToolCall name")
	fs.StringVar(&cfg.token, "token", "", "raw gateway stream token (obtained out-of-band from the runner; not persisted in the ToolCall)")
	fs.StringVar(&cfg.outDir, "out", ".", "directory for stdout/stderr/exit files")
	fs.StringVar(&cfg.stdinStr, "stdin", "", "send this string as stdin then close-send")
	fs.StringVar(&cfg.stdinFile, "stdin-file", "", "send this file as stdin then close-send")
	fs.DurationVar(&cfg.timeout, "timeout", 60*time.Second, "overall RPC timeout")
	return cmd
}

func run(cfg *config) error {
	if cfg.name == "" || cfg.token == "" {
		log.Fatal("--toolcall and --token are required")
	}
	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", cfg.outDir, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	conn, err := grpc.NewClient(cfg.gatewayAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial %s: %v", cfg.gatewayAddr, err)
	}
	defer conn.Close()

	stream, err := gatewayv1.NewGatewayClient(conn).Stream(ctx)
	if err != nil {
		log.Fatalf("open stream: %v", err)
	}

	// First message MUST be Hello.
	if err := stream.Send(&gatewayv1.Message{
		Kind: &gatewayv1.Message_Hello{Hello: &gatewayv1.Hello{
			Token:        cfg.token,
			Namespace:    cfg.ns,
			ToolCallName: cfg.name,
		}},
	}); err != nil {
		log.Fatalf("send hello: %v", err)
	}

	// Optionally pipe stdin.
	var stdinBytes []byte
	switch {
	case cfg.stdinStr != "":
		stdinBytes = []byte(cfg.stdinStr)
	case cfg.stdinFile != "":
		b, rerr := os.ReadFile(cfg.stdinFile)
		if rerr != nil {
			log.Fatalf("read --stdin-file %s: %v", cfg.stdinFile, rerr)
		}
		stdinBytes = b
	}
	if len(stdinBytes) > 0 {
		if err := stream.Send(&gatewayv1.Message{
			Kind: &gatewayv1.Message_Stdin{Stdin: &gatewayv1.BytesChunk{Data: stdinBytes}},
		}); err != nil {
			log.Fatalf("send stdin: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		log.Fatalf("close send: %v", err)
	}

	stdoutF, err := os.Create(filepath.Join(cfg.outDir, "stdout"))
	if err != nil {
		log.Fatal(err)
	}
	defer stdoutF.Close()
	stderrF, err := os.Create(filepath.Join(cfg.outDir, "stderr"))
	if err != nil {
		log.Fatal(err)
	}
	defer stderrF.Close()

	var exitCode int32 = -1
	var exitReason string
	gotExit := false
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			// Exit is the documented terminal message in gateway.proto. The
			// server may close the stream with an error after sending Exit
			// (e.g. ErrClosedPipe on the stdout pipe once the process exits);
			// treat that as a clean shutdown.
			if gotExit {
				break
			}
			log.Fatalf("recv: %v", rerr)
		}
		switch m := msg.Kind.(type) {
		case *gatewayv1.Message_Stdout:
			if _, werr := stdoutF.Write(m.Stdout.Data); werr != nil {
				log.Fatalf("write stdout: %v", werr)
			}
		case *gatewayv1.Message_Stderr:
			if _, werr := stderrF.Write(m.Stderr.Data); werr != nil {
				log.Fatalf("write stderr: %v", werr)
			}
		case *gatewayv1.Message_Exit:
			exitCode = m.Exit.Code
			exitReason = m.Exit.Reason
			gotExit = true
		}
	}

	exitBody := fmt.Sprintf("code=%d\nreason=%s\n", exitCode, exitReason)
	if err := os.WriteFile(filepath.Join(cfg.outDir, "exit"), []byte(exitBody), 0o644); err != nil {
		log.Fatalf("write exit: %v", err)
	}

	fmt.Printf("toolcall=%s exit=%d reason=%q out=%s\n", cfg.name, exitCode, exitReason, cfg.outDir)
	if exitCode != 0 {
		os.Exit(1)
	}
	return nil
}
