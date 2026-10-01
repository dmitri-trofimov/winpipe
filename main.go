package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"time"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("winpipe: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: winpipe <listen|connect|serve> [options]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch args[0] {
	case "listen":
		fs := flag.NewFlagSet("listen", flag.ContinueOnError)
		address := fs.String("listen", "127.0.0.1:9222", "local TCP listening address (loopback only)")
		target := fs.String("target", "127.0.0.1:9222", "destination as seen from Windows")
		helper := fs.String("helper", "", "path to the Windows winpipe.exe")
		timeout := fs.Duration("dial-timeout", 10*time.Second, "Windows destination connection timeout")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if fs.NArg() != 0 || *helper == "" || *timeout <= 0 {
			return errors.New("listen requires --helper, a positive --dial-timeout, and no positional arguments")
		}
		return listen(ctx, *address, *target, *helper, *timeout)
	case "connect":
		fs := flag.NewFlagSet("connect", flag.ContinueOnError)
		target := fs.String("target", "127.0.0.1:9222", "TCP destination")
		timeout := fs.Duration("dial-timeout", 10*time.Second, "destination connection timeout")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if fs.NArg() != 0 || *timeout <= 0 {
			return errors.New("connect requires a positive --dial-timeout and no positional arguments")
		}
		dialer := net.Dialer{Timeout: *timeout}
		conn, err := dialer.DialContext(ctx, "tcp", *target)
		if err != nil {
			return fmt.Errorf("connect to %s: %w", *target, err)
		}
		return relay(ctx, &pipeStream{reader: os.Stdin, writer: os.Stdout}, conn)
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		address := fs.String("listen", "127.0.0.1:9223", "local TCP listening address (loopback only)")
		target := fs.String("target", "127.0.0.1:9222", "local TCP destination")
		timeout := fs.Duration("dial-timeout", 10*time.Second, "destination connection timeout")
		logPath := fs.String("log-file", "", "append diagnostics to this file instead of stderr")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if fs.NArg() != 0 || *timeout <= 0 || *address == *target {
			return errors.New("serve requires a positive --dial-timeout, distinct listener and target, and no positional arguments")
		}
		if *logPath != "" {
			file, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				return fmt.Errorf("open log file: %w", err)
			}
			// Keep the file open until process exit so main can log fatal errors.
			log.SetOutput(file)
		}
		return listen(ctx, *address, *target, "", *timeout)
	case "help", "--help", "-h":
		fmt.Fprintln(os.Stdout, "Usage: winpipe <listen|connect|serve> [options]\nRun winpipe <command> -h for options.")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// validateListen deliberately rejects wildcard and non-loopback listeners.
func validateListen(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen address must use a literal loopback IP, such as 127.0.0.1 or ::1")
	}
	return nil
}

func listen(ctx context.Context, address, target, helper string, timeout time.Duration) error {
	if err := validateListen(address); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	if helper == "" {
		log.Printf("winpipe: local TCP listener %s -> %s", ln.Addr(), target)
	} else {
		log.Printf("winpipe: %s -> Windows %s", ln.Addr(), target)
	}
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			cancel()
			if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			var err error
			if helper == "" {
				err = proxy(ctx, conn, target, timeout)
			} else {
				err = forward(ctx, conn, target, helper, timeout)
			}
			if err != nil && ctx.Err() == nil {
				log.Printf("winpipe: connection: %v", err)
			}
		}()
	}
}

func proxy(ctx context.Context, client net.Conn, target string, timeout time.Duration) error {
	defer client.Close()
	dialer := net.Dialer{Timeout: timeout}
	destination, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", target, err)
	}
	return relay(ctx, client, destination)
}

func forward(ctx context.Context, conn net.Conn, target, helper string, timeout time.Duration) error {
	defer conn.Close()
	cmd := exec.CommandContext(ctx, helper, "connect", "--target", target, "--dial-timeout", timeout.String())
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer output.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Windows helper: %w", err)
	}
	relayErr := relay(ctx, conn, &pipeStream{reader: output, writer: input})
	waitErr := cmd.Wait()
	return errors.Join(relayErr, waitErr)
}

type pipeStream struct {
	reader io.ReadCloser
	writer io.Writer
}

func (p *pipeStream) Read(b []byte) (int, error)  { return p.reader.Read(b) }
func (p *pipeStream) Write(b []byte) (int, error) { return p.writer.Write(b) }
func (p *pipeStream) CloseWrite() error {
	if closer, ok := p.writer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
func (p *pipeStream) Close() error {
	return errors.Join(p.reader.Close(), p.CloseWrite())
}

// relay preserves a client write-half-close. Destination EOF terminates the
// session and unblocks any outstanding client read. No bytes are decoded.
func relay(ctx context.Context, client io.ReadWriteCloser, destination io.ReadWriteCloser) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
			destination.Close()
		case <-done:
		}
	}()
	defer close(done)
	upload := make(chan error, 1)
	go func() {
		_, err := io.Copy(destination, client)
		if half, ok := destination.(interface{ CloseWrite() error }); ok {
			err = errors.Join(err, half.CloseWrite())
		}
		if err != nil {
			destination.Close()
			client.Close()
		}
		upload <- err
	}()
	_, downloadErr := io.Copy(client, destination)
	client.Close()
	destination.Close()
	uploadErr := <-upload
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(ignoreClosed(downloadErr), ignoreClosed(uploadErr))
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}
