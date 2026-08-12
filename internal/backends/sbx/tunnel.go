package sbxbackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

var tunnelHello = []byte("E2BTUN1\x00")

const tunnelAssignmentByte = byte(1)

// TunnelSpec is passed to the static in-VM helper. RelayAddress must be
// reachable from the microVM, while PublicPort is advertised to the E2B SDK.
type TunnelSpec struct {
	RelayAddress string      `json:"relay_address"`
	Token        string      `json:"token"`
	TargetPort   int         `json:"target_port"`
	PublicPort   int         `json:"public_port"`
	Connections  int         `json:"connections"`
	OnError      func(error) `json:"-"`
}

type TunnelRelay struct {
	listener  net.Listener
	token     string
	available chan net.Conn

	mu          sync.Mutex
	vmConns     map[net.Conn]struct{}
	bridgeConns map[net.Conn]struct{}
	closed      bool
	closeCh     chan struct{}
	wg          sync.WaitGroup
}

func newTunnelToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate tunnel token: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func NewTunnelRelay(bindHost string, port int, token string) (*TunnelRelay, error) {
	if token == "" {
		return nil, fmt.Errorf("tunnel token is required")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, fmt.Sprintf("%d", port)))
	if err != nil {
		return nil, fmt.Errorf("listen for tunnel relay: %w", err)
	}
	relay := &TunnelRelay{
		listener:    listener,
		token:       token,
		available:   make(chan net.Conn, 64),
		vmConns:     map[net.Conn]struct{}{},
		bridgeConns: map[net.Conn]struct{}{},
		closeCh:     make(chan struct{}),
	}
	relay.wg.Add(1)
	go relay.acceptLoop()
	return relay, nil
}

func (r *TunnelRelay) Port() int {
	address, _ := r.listener.Addr().(*net.TCPAddr)
	if address == nil {
		return 0
	}
	return address.Port
}

func (r *TunnelRelay) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.closeCh)
	connections := make([]net.Conn, 0, len(r.vmConns)+len(r.bridgeConns))
	for connection := range r.vmConns {
		connections = append(connections, connection)
	}
	for connection := range r.bridgeConns {
		connections = append(connections, connection)
	}
	r.mu.Unlock()

	err := r.listener.Close()
	for _, connection := range connections {
		_ = connection.Close()
	}
	r.wg.Wait()
	return err
}

func (r *TunnelRelay) acceptLoop() {
	defer r.wg.Done()
	for {
		connection, err := r.listener.Accept()
		if err != nil {
			if r.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.acceptConnection(connection)
		}()
	}
}

func (r *TunnelRelay) acceptConnection(connection net.Conn) {
	_ = connection.SetReadDeadline(time.Now().Add(15 * time.Second))
	prefix := make([]byte, len(tunnelHello))
	if _, err := io.ReadFull(connection, prefix); err != nil {
		_ = connection.Close()
		return
	}
	if bytes.Equal(prefix, tunnelHello) {
		token := make([]byte, len(r.token))
		if _, err := io.ReadFull(connection, token); err != nil || string(token) != r.token {
			_ = connection.Close()
			return
		}
		_ = connection.SetReadDeadline(time.Time{})
		r.registerVMConnection(connection)
		return
	}
	_ = connection.SetReadDeadline(time.Time{})
	r.serveUserConnection(&prefixedConn{Conn: connection, prefix: bytes.NewReader(prefix)})
}

func (r *TunnelRelay) registerVMConnection(connection net.Conn) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = connection.Close()
		return
	}
	r.vmConns[connection] = struct{}{}
	r.mu.Unlock()

	select {
	case r.available <- connection:
	case <-r.closeCh:
		_ = connection.Close()
	}
}

func (r *TunnelRelay) serveUserConnection(user net.Conn) {
	defer user.Close()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case vm := <-r.available:
			if err := r.assignVMConnection(vm); err != nil {
				continue
			}
			if !r.registerBridgeConnections(user, vm) {
				_ = vm.Close()
				return
			}
			bridgeConnections(user, vm)
			r.unregisterBridgeConnections(user, vm)
			_ = vm.Close()
			return
		case <-deadline.C:
			return
		case <-r.closeCh:
			return
		}
	}
}

func (r *TunnelRelay) assignVMConnection(connection net.Conn) error {
	r.mu.Lock()
	delete(r.vmConns, connection)
	r.mu.Unlock()
	if _, err := connection.Write([]byte{tunnelAssignmentByte}); err != nil {
		_ = connection.Close()
		return err
	}
	return nil
}

func (r *TunnelRelay) registerBridgeConnections(connections ...net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	for _, connection := range connections {
		if connection != nil {
			r.bridgeConns[connection] = struct{}{}
		}
	}
	return true
}

func (r *TunnelRelay) unregisterBridgeConnections(connections ...net.Conn) {
	r.mu.Lock()
	for _, connection := range connections {
		delete(r.bridgeConns, connection)
	}
	r.mu.Unlock()
}

func (r *TunnelRelay) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// RunTunnel maintains reverse connections from a microVM to one host relay.
// Each connection carries exactly one proxied request, which avoids protocol
// multiplexing inside the sandbox and keeps the data plane transparent.
func RunTunnel(ctx context.Context, spec TunnelSpec) error {
	if spec.RelayAddress == "" || spec.Token == "" || spec.TargetPort <= 0 || spec.TargetPort > 65535 {
		return fmt.Errorf("invalid tunnel configuration")
	}
	if spec.Connections <= 0 {
		spec.Connections = 1
	}

	var workers sync.WaitGroup
	for range spec.Connections {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runTunnelWorker(ctx, spec)
		}()
	}
	<-ctx.Done()
	workers.Wait()
	return nil
}

func runTunnelWorker(ctx context.Context, spec TunnelSpec) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := runTunnelConnection(ctx, spec); err != nil && ctx.Err() == nil {
			if spec.OnError != nil {
				spec.OnError(err)
			}
			if !waitForContext(ctx, time.Second) {
				return
			}
		}
	}
}

func runTunnelConnection(ctx context.Context, spec TunnelSpec) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", spec.RelayAddress)
	if err != nil {
		return fmt.Errorf("dial tunnel relay %s: %w", spec.RelayAddress, err)
	}
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	defer connection.Close()

	if _, err := connection.Write(append(append([]byte{}, tunnelHello...), []byte(spec.Token)...)); err != nil {
		return fmt.Errorf("write tunnel hello: %w", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(30 * time.Second))
	assignment := []byte{0}
	if _, err := io.ReadFull(connection, assignment); err != nil {
		return fmt.Errorf("read tunnel assignment: %w", err)
	}
	if assignment[0] != tunnelAssignmentByte {
		return fmt.Errorf("unexpected relay assignment")
	}
	_ = connection.SetReadDeadline(time.Time{})

	target, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", spec.TargetPort)))
	if err != nil {
		return fmt.Errorf("dial guest port %d: %w", spec.TargetPort, err)
	}
	defer target.Close()
	bridgeConnections(connection, target)
	return nil
}

func waitForContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func bridgeConnections(left, right net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(left, right)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(right, left)
		done <- struct{}{}
	}()
	<-done
	_ = left.Close()
	_ = right.Close()
	<-done
}

type prefixedConn struct {
	net.Conn
	prefix *bytes.Reader
}

func (c *prefixedConn) Read(data []byte) (int, error) {
	if c.prefix.Len() > 0 {
		return c.prefix.Read(data)
	}
	return c.Conn.Read(data)
}
