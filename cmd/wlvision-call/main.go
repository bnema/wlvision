// Command wlvision-call exchanges exactly one request with the resident
// controller over the session's control socket and exits.
//
// The outer CLI runs it inside the container as the control UID, once per
// operation. It exists so that nothing but the resident controller ever holds
// the privileged connection: the caller sends one document, receives one
// document, prints it, and exits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/rpc"
	"github.com/bnema/wlvision/internal/session"
)

// DefaultTimeout bounds one exchange. A controller that stopped answering must
// not leave a caller hanging inside a container exec.
const DefaultTimeout = 30 * time.Second

// Caller sends one request and reads one reply.
type Caller interface {
	Call(ctx context.Context, request []byte) ([]byte, error)
}

// Dialer opens one connection to the resident controller.
type Dialer func(ctx context.Context, socketPath string) (Caller, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, rpcDialer))
}

// rpcDialer is the production dialer: one connection, one request.
func rpcDialer(ctx context.Context, socketPath string) (Caller, error) {
	return rpc.Dial(ctx, socketPath)
}

// run executes one call. It is the whole command except process wiring.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, dial Dialer) int {
	flags := flag.NewFlagSet("wlvision-call", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: wlvision-call [--socket PATH] [--params JSON] [--timeout DURATION] OPERATION")
		flags.PrintDefaults()
	}

	socket := flags.String("socket", session.AgentSocket, "the resident controller's socket")
	params := flags.String("params", "", "operation arguments as one JSON object")
	timeout := flags.Duration("timeout", DefaultTimeout, "how long the exchange may take")

	if err := flags.Parse(args); err != nil {
		return reportUsage(stderr, "%v", err)
	}

	operation := flags.Arg(0)
	if operation == "" {
		return reportUsage(stderr, "an operation is required")
	}
	if flags.NArg() > 1 {
		return reportUsage(stderr, "exactly one operation is expected, got %d arguments", flags.NArg())
	}

	request := agentapi.Request{Operation: operation}
	if *params != "" {
		if !json.Valid([]byte(*params)) {
			return reportUsage(stderr, "the arguments are not valid JSON")
		}
		request.Params = json.RawMessage(*params)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return reportFailure(stderr, result.NewFailure(result.CodeUsageError, operation,
			"cannot encode the request: %v", err))
	}

	callCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	conn, err := dial(callCtx, *socket)
	if err != nil {
		return reportFailure(stderr, failure(operation, err))
	}
	defer func() { _ = closeCaller(conn) }()

	reply, err := conn.Call(callCtx, encoded)
	if err != nil {
		return reportFailure(stderr, failure(operation, err))
	}

	if _, err := fmt.Fprintf(stdout, "%s\n", reply); err != nil {
		return reportFailure(stderr, result.NewFailure(result.CodeSessionNotReady, operation,
			"cannot write the reply: %v", err))
	}
	return 0
}

// closeCaller releases the connection when the caller has a Close method.
func closeCaller(caller Caller) error {
	if closer, ok := caller.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// failure reports what went wrong, keeping the code the transport or the
// controller assigned when either gave one.
func failure(operation string, err error) *result.Failure {
	var typed *result.Failure
	if errors.As(err, &typed) {
		return typed
	}

	code := result.CodeSessionNotReady
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		code = result.CodeWaitTimeout
	}
	return result.NewFailure(code, operation, "%v", err)
}

// reportUsage reports a command-line problem, which never reaches the session.
func reportUsage(stderr io.Writer, format string, args ...any) int {
	return reportFailure(stderr, result.NewFailure(result.CodeUsageError, "call", format, args...))
}

// reportFailure writes one failure document for the outer CLI to read and
// returns the exit code the contract assigns.
func reportFailure(stderr io.Writer, failure *result.Failure) int {
	envelope := result.Fail[any](failure.Operation, "", failure)
	if err := result.RenderJSON(stderr, envelope); err != nil {
		fmt.Fprintf(stderr, "wlvision-call: %v\n", err)
		return 1
	}
	return failure.Code.ExitCode()
}
