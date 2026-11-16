// Package emulator talks to a running Android emulator over its gRPC control
// port, as Android Studio's embedded emulator does: frames straight from the
// emulator's display, and touches and keys straight into it, with no adb and
// no encoder inside the emulated phone. It speaks just the few calls it
// needs, gRPC framed by hand over the HTTP/2 the host already has.
package emulator

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2"
)

const service = "/android.emulation.control.EmulatorController/"

// Endpoint is where one running emulator answers, from its discovery file.
type Endpoint struct {
	Port  int
	Token string
}

// Find reads the discovery file of the running emulator whose console port
// is consolePort (5554 for emulator-5554).
func Find(consolePort int) (Endpoint, error) {
	for _, dir := range runningDirs() {
		files, _ := filepath.Glob(filepath.Join(dir, "pid_*.ini"))
		for _, file := range files {
			values := readIni(file)
			if values["port.serial"] != strconv.Itoa(consolePort) {
				continue
			}
			port, err := strconv.Atoi(values["grpc.port"])
			if err != nil || port == 0 {
				return Endpoint{}, errors.New("the emulator has no gRPC port")
			}
			return Endpoint{Port: port, Token: values["grpc.token"]}, nil
		}
	}
	return Endpoint{}, errors.New("no running emulator's gRPC port found")
}

// runningDirs are where emulators write their discovery files: the Mac's
// caches, Windows' temp folder (%TEMP%\avd\running), and on Linux the
// runtime folder or /tmp/android-$USER.
func runningDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{filepath.Join(os.TempDir(), "avd", "running")}
	}
	home, _ := os.UserHomeDir()
	var dirs []string
	if runtime.GOOS == "darwin" {
		dirs = append(dirs, filepath.Join(home, "Library/Caches/TemporaryItems/avd/running"))
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		dirs = append(dirs, filepath.Join(dir, "avd/running"))
	}
	return append(dirs, filepath.Join(os.TempDir(), "android-"+os.Getenv("USER"), "avd/running"))
}

func readIni(path string) map[string]string {
	values := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return values
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		if k, v, ok := strings.Cut(lines.Text(), "="); ok {
			values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return values
}

// Client is one emulator's control port.
type Client struct {
	base  string
	token string
	http  *http.Client
}

func Dial(e Endpoint) *Client {
	// The port speaks HTTP/2 without TLS, on loopback.
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	return &Client{base: fmt.Sprintf("http://127.0.0.1:%d", e.Port), token: e.Token, http: &http.Client{Transport: transport}}
}

func (c *Client) request(ctx context.Context, method string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+service+method, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("emulator %s: HTTP %d", method, resp.StatusCode)
	}
	return resp, nil
}

// frame is a gRPC message on the wire: uncompressed, its length, itself.
func frame(message []byte) []byte {
	out := make([]byte, 5+len(message))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(message)))
	copy(out[5:], message)
	return out
}

// readFrame reads one gRPC message; io.EOF at the end of the stream.
func readFrame(r io.Reader) ([]byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if head[0] != 0 {
		return nil, errors.New("emulator sent a compressed message")
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > 64<<20 {
		return nil, fmt.Errorf("emulator sent a %d-byte message", n)
	}
	message := make([]byte, n)
	_, err := io.ReadFull(r, message)
	return message, err
}

// status turns a finished call's trailers into its error.
func status(resp *http.Response) error {
	code := resp.Trailer.Get("Grpc-Status")
	if code == "" {
		code = resp.Header.Get("Grpc-Status")
	}
	if code == "" || code == "0" {
		return nil
	}
	return fmt.Errorf("emulator: gRPC status %s: %s", code, resp.Trailer.Get("Grpc-Message"))
}

// Stream runs a server-streaming method, handing each message to each.
func (c *Client) Stream(ctx context.Context, method string, message []byte, each func([]byte) error) error {
	resp, err := c.request(ctx, method, bytes.NewReader(frame(message)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for {
		m, err := readFrame(resp.Body)
		if errors.Is(err, io.EOF) {
			return status(resp)
		}
		if err != nil {
			return err
		}
		if err := each(m); err != nil {
			return err
		}
	}
}

// Sender is a client-streaming call open for messages, sent in order.
type Sender struct {
	mu   sync.Mutex
	pipe *io.PipeWriter
	done chan error
}

// Open starts a client-streaming method; Send each message, then Close.
func (c *Client) Open(ctx context.Context, method string) *Sender {
	r, w := io.Pipe()
	s := &Sender{pipe: w, done: make(chan error, 1)}
	go func() {
		resp, err := c.request(ctx, method, r)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			err = status(resp)
		}
		r.CloseWithError(err)
		s.done <- err
	}()
	return s
}

func (s *Sender) Send(message []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.pipe.Write(frame(message))
	return err
}

func (s *Sender) Close() error {
	s.pipe.Close()
	return <-s.done
}
