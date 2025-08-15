package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/kralicky/protols/pkg/lsprpc"
	"github.com/kralicky/tools-lite/pkg/event"
	"github.com/kralicky/tools-lite/pkg/event/core"
	"github.com/kralicky/tools-lite/pkg/event/keys"
	"github.com/kralicky/tools-lite/pkg/event/label"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"
	"github.com/spf13/cobra"
)

// stdioConn implements a connection over stdin/stdout
type stdioConn struct {
	reader io.Reader
	writer io.Writer
}

func (s *stdioConn) Read(p []byte) (n int, err error) {
	return s.reader.Read(p)
}

func (s *stdioConn) Write(p []byte) (n int, err error) {
	return s.writer.Write(p)
}

func (s *stdioConn) Close() error {
	return nil
}

func (s *stdioConn) LocalAddr() net.Addr {
	return &stdioAddr{}
}

func (s *stdioConn) RemoteAddr() net.Addr {
	return &stdioAddr{}
}

func (s *stdioConn) SetDeadline(t time.Time) error {
	return nil
}

func (s *stdioConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (s *stdioConn) SetWriteDeadline(t time.Time) error {
	return nil
}

type stdioAddr struct{}

func (s *stdioAddr) Network() string {
	return "stdio"
}

func (s *stdioAddr) String() string {
	return "stdio"
}

// ServeCmd represents the serve command
func BuildServeCmd() *cobra.Command {
	var pipe string
	var stdio bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the language server",
		RunE: func(cmd *cobra.Command, args []string) error {
			// When using stdio, silence all logging AND redirect command output to avoid interfering with LSP communication
			if stdio {
				// Disable all logging in stdio mode
				slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{})))
				// Disable event exporter
				event.SetExporter(func(ctx context.Context, e core.Event, lm label.Map) context.Context {
					return ctx
				})
				// Redirect cobra command output to stderr to avoid interfering with stdin/stdout LSP communication
				cmd.SetOut(os.Stderr)
				cmd.SetErr(os.Stderr)
			} else {
				slog.SetDefault(slog.New(slog.NewTextHandler(cmd.OutOrStderr(), &slog.HandlerOptions{
					AddSource: true,
					Level:     slog.LevelDebug,
				})))
				var eventMu sync.Mutex
				event.SetExporter(func(ctx context.Context, e core.Event, lm label.Map) context.Context {
					eventMu.Lock()
					defer eventMu.Unlock()
					if event.IsError(e) {
						if err := keys.Err.Get(e); errors.Is(err, context.Canceled) {
							return ctx
						}
						var args []any
						for i := 0; e.Valid(i); i++ {
							l := e.Label(i)
							if !l.Valid() || l.Key() == keys.Msg {
								continue
							}
							key := l.Key()
							var val bytes.Buffer
							key.Format(&val, nil, l)
							args = append(args, l.Key().Name(), val.String())
						}
						slog.Error(keys.Msg.Get(e), args...)
					}
					return ctx
				})
			}

			var stream jsonrpc2.Stream
			if stdio {
				// Use stdin/stdout for communication
				stream = jsonrpc2.NewHeaderStream(&stdioConn{
					reader: os.Stdin,
					writer: os.Stdout,
				})
			} else {
				if pipe == "" {
					return errors.New("either --stdio or --pipe must be specified")
				}
				cc, err := net.Dial("unix", pipe)
				if err != nil {
					return err
				}
				stream = jsonrpc2.NewHeaderStream(cc)
			}

			conn := jsonrpc2.NewConn(stream)
			ss := lsprpc.NewStreamServer()
			err := ss.ServeStream(cmd.Context(), conn)
			if stdio && err != nil {
				// In stdio mode, don't let Cobra print error messages to stdout
				os.Exit(1)
			}
			return err
		},
	}

	cmd.Flags().StringVar(&pipe, "pipe", "", "socket name to listen on")
	cmd.Flags().BoolVar(&stdio, "stdio", false, "use stdin/stdout for communication")

	return cmd
}
