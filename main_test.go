package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// The test executable doubles as a native helper, exercising real subprocess
// pipes without requiring Windows interop on the test machine.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "connect" {
		if err := run(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestValidateListen(t *testing.T) {
	for _, address := range []string{"127.0.0.1:9222", "[::1]:8080", "127.0.0.2:0"} {
		if err := validateListen(address); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
	for _, address := range []string{":9222", "0.0.0.0:9222", "[::]:9222", "192.168.1.1:22", "localhost:9222", "invalid"} {
		if err := validateListen(address); err == nil {
			t.Errorf("accepted unsafe or invalid address %s", address)
		}
	}
}

func tcpPair(t *testing.T) (*net.TCPConn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.Accept()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	client.SetDeadline(time.Now().Add(15 * time.Second))
	return client.(*net.TCPConn), server
}

func TestForwardBinaryConcurrentAndHalfClose(t *testing.T) {
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	const count = 8
	serverErrors := make(chan error, count)
	go func() {
		for i := 0; i < count; i++ {
			conn, err := target.Accept()
			if err != nil {
				serverErrors <- err
				continue
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(15 * time.Second))
				// Respond only after EOF: this requires half-close propagation
				// through both subprocess pipes and the helper's TCP socket.
				data, err := io.ReadAll(conn)
				if err == nil {
					_, err = conn.Write(data)
				}
				serverErrors <- err
			}()
		}
	}()
	var clients sync.WaitGroup
	for i := 0; i < count; i++ {
		client, bridge := tcpPair(t)
		clients.Add(1)
		go func(seed byte) {
			defer clients.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- forward(ctx, bridge, target.Addr().String(), helper, time.Second) }()
			payload := make([]byte, 256*1024)
			for j := range payload {
				payload[j] = byte(j) + seed
			}
			if _, err := client.Write(payload); err != nil {
				t.Error(err)
			}
			if err := client.CloseWrite(); err != nil {
				t.Error(err)
			}
			response, err := io.ReadAll(client)
			if err != nil {
				t.Error(err)
			}
			if !bytes.Equal(response, payload) {
				t.Errorf("binary response differs: got %d bytes, want %d", len(response), len(payload))
			}
			if err := <-result; err != nil {
				t.Error(err)
			}
		}(byte(i))
	}
	clients.Wait()
	for i := 0; i < count; i++ {
		if err := <-serverErrors; err != nil {
			t.Error(err)
		}
	}
}

func TestRelayCancellation(t *testing.T) {
	_, client := tcpPair(t)
	_, target := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- relay(ctx, client, target) }()
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop on cancellation")
	}
}

func TestForwardMissingHelper(t *testing.T) {
	client, bridge := tcpPair(t)
	if err := forward(context.Background(), bridge, "127.0.0.1:9222", "/nonexistent/winpipe.exe", time.Second); err == nil {
		t.Fatal("missing helper was accepted")
	}
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("client not closed after helper failure: %v", err)
	}
}

func TestForwardCancellation(t *testing.T) {
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := target.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	_, bridge := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- forward(ctx, bridge, target.Addr().String(), helper, time.Second) }()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not connect")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancellation was not reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not stop on cancellation")
	}
}

func TestForwardStreaming(t *testing.T) {
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverResult := make(chan error, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, err = io.Copy(conn, conn)
		serverResult <- err
	}()
	client, bridge := tcpPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- forward(ctx, bridge, target.Addr().String(), helper, time.Second) }()
	for i := 0; i < 10; i++ {
		message := []byte{0, byte(i), 255, '\r', '\n'}
		if _, err := client.Write(message); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, len(message))
		if _, err := io.ReadFull(client, response); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(message, response) {
			t.Fatal("streaming response differs")
		}
	}
	client.CloseWrite()
	io.Copy(io.Discard, client)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestProxyBinaryAndHalfClose(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverResult := make(chan error, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		data, err := io.ReadAll(conn)
		if err == nil {
			_, err = conn.Write(data)
		}
		serverResult <- err
	}()
	client, bridge := tcpPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- proxy(ctx, bridge, target.Addr().String(), time.Second) }()
	payload := bytes.Repeat([]byte{0, 255, 128, '\r', '\n'}, 65536)
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	client.CloseWrite()
	response, err := io.ReadAll(client)
	if err != nil || !bytes.Equal(payload, response) {
		t.Fatalf("proxy response mismatch: %v, got %d bytes", err, len(response))
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestServeRejectsSelfForwarding(t *testing.T) {
	if err := run([]string{"serve", "--listen", "127.0.0.1:9223", "--target", "127.0.0.1:9223"}); err == nil {
		t.Fatal("serve accepted forwarding to the exact listening address")
	}
}

func TestForwardConnectionRefused(t *testing.T) {
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	_, bridge := tcpPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := forward(ctx, bridge, address, helper, time.Second); err == nil {
		t.Fatal("connection refusal was not reported")
	}
	if ctx.Err() != nil {
		t.Fatal("helper failure hung until context timeout")
	}
}
