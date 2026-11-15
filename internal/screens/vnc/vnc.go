// Package vnc is a small RFB (VNC) client for Apple's Screen Sharing on the
// same Mac: it signs in with the Mac account (Apple's own security type,
// never VNC's DES password, which would expose the account's password), keeps
// the screen as 32-bit BGRA
// pixels from raw, CopyRect and resize updates, and sends pointer and key
// events. It speaks only to loopback, so raw pixels cost nothing to carry.
package vnc

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"time"
)

// Client is one signed-in connection.
type Client struct {
	conn   net.Conn
	r      *bufio.Reader
	writeM sync.Mutex

	// Screen is the screen's pixels, BGRA, Width*4 bytes a row; only Run's
	// frame callback may read it.
	Screen        []byte
	Width, Height int
	// size guards Width and Height for Size, which input reads elsewhere.
	size sync.Mutex
}

// Size is the screen's size in pixels now.
func (c *Client) Size() (w, h int) {
	c.size.Lock()
	defer c.size.Unlock()
	return c.Width, c.Height
}

// The security types this client takes, and the encodings it asks for.
const (
	securityApple   = 30 // Apple's account sign-in: Diffie-Hellman, then AES
	encodingRaw     = 0
	encodingCopy    = 1
	encodingResize  = -223
	versionReply    = "RFB 003.008\n"
	msgUpdate       = 0
	msgColourMap    = 1
	msgBell         = 2
	msgCutText      = 3
	msgSetFormat    = 0
	msgSetEncodings = 2
	msgRequest      = 3
	msgKey          = 4
	msgPointer      = 5
)

// ErrNoSignIn is a server that doesn't offer Apple's account sign-in, and
// ErrRefused one that refused the user and password.
var (
	ErrNoSignIn = errors.New("Screen Sharing doesn't offer sign-in with a Mac account")
	ErrRefused  = errors.New("Screen Sharing refused the password")
)

// Dial connects to addr and signs in as the Mac account user with password.
func Dial(ctx context.Context, addr, user, password string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, 1<<20)}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.handshake(user, password); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return c, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) handshake(user, password string) error {
	version := make([]byte, 12)
	if _, err := io.ReadFull(c.r, version); err != nil {
		return fmt.Errorf("vnc: version: %w", err)
	}
	if _, err := c.conn.Write([]byte(versionReply)); err != nil {
		return err
	}
	count, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	if count == 0 {
		return c.reason()
	}
	types := make([]byte, count)
	if _, err := io.ReadFull(c.r, types); err != nil {
		return err
	}
	if !contains(types, securityApple) {
		return ErrNoSignIn
	}
	if err := c.signInApple(user, password); err != nil {
		return err
	}
	var result uint32
	if err := binary.Read(c.r, binary.BigEndian, &result); err != nil {
		return err
	}
	if result != 0 {
		return ErrRefused
	}
	if _, err := c.conn.Write([]byte{1}); err != nil { // shared: other viewers stay
		return err
	}
	var init struct {
		Width, Height uint16
		Format        [16]byte
		NameLength    uint32
	}
	if err := binary.Read(c.r, binary.BigEndian, &init); err != nil {
		return err
	}
	if _, err := c.r.Discard(int(init.NameLength)); err != nil {
		return err
	}
	c.resize(int(init.Width), int(init.Height))
	// 32 bits a pixel, little-endian, red at bit 16: B, G, R, X in memory.
	format := []byte{msgSetFormat, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	encodings := []int32{encodingRaw, encodingCopy, encodingResize}
	message := []byte{msgSetEncodings, 0}
	message = binary.BigEndian.AppendUint16(message, uint16(len(encodings)))
	for _, e := range encodings {
		message = binary.BigEndian.AppendUint32(message, uint32(e))
	}
	return c.write(append(format, message...))
}

// signInApple is Apple's account sign-in: the server sends a Diffie-Hellman
// group and its public key; the MD5 of the shared secret keys AES-128-ECB,
// which encrypts the user and password, sent with the client's public key.
func (c *Client) signInApple(user, password string) error {
	if _, err := c.conn.Write([]byte{securityApple}); err != nil {
		return err
	}
	var head struct{ Generator, KeyLength uint16 }
	if err := binary.Read(c.r, binary.BigEndian, &head); err != nil {
		return err
	}
	prime := make([]byte, head.KeyLength)
	serverKey := make([]byte, head.KeyLength)
	if _, err := io.ReadFull(c.r, prime); err != nil {
		return err
	}
	if _, err := io.ReadFull(c.r, serverKey); err != nil {
		return err
	}
	credentials, clientKey, err := AppleCredentials(head.Generator, prime, serverKey, user, password)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(append(credentials, clientKey...))
	return err
}

// AppleCredentials is the client's half of Apple's sign-in: the encrypted
// user and password, and the client's public key, both for the server.
func AppleCredentials(generator uint16, prime, serverKey []byte, user, password string) (credentials, clientKey []byte, err error) {
	if len(user) > 63 || len(password) > 63 {
		return nil, nil, errors.New("vnc: a user or password over 63 bytes")
	}
	p := new(big.Int).SetBytes(prime)
	private, err := rand.Int(rand.Reader, p)
	if err != nil {
		return nil, nil, err
	}
	public := new(big.Int).Exp(big.NewInt(int64(generator)), private, p)
	shared := new(big.Int).Exp(new(big.Int).SetBytes(serverKey), private, p)
	key := md5.Sum(shared.FillBytes(make([]byte, len(prime))))
	// User, then password, each in 64 bytes, zero-terminated, the rest random.
	plain := make([]byte, 128)
	if _, err := rand.Read(plain); err != nil {
		return nil, nil, err
	}
	copy(plain, append([]byte(user), 0))
	copy(plain[64:], append([]byte(password), 0))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, nil, err
	}
	credentials = make([]byte, 128)
	for i := 0; i < 128; i += aes.BlockSize {
		block.Encrypt(credentials[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return credentials, public.FillBytes(make([]byte, len(prime))), nil
}

// Run keeps the screen current, calling frame after each update until ctx
// ends or the connection fails. frame runs on Run's goroutine.
func (c *Client) Run(ctx context.Context, frame func()) error {
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	if err := c.request(false); err != nil {
		return err
	}
	for {
		kind, err := c.r.ReadByte()
		if err != nil {
			return c.ended(ctx, err)
		}
		switch kind {
		case msgUpdate:
			if err := c.update(); err != nil {
				return c.ended(ctx, err)
			}
			frame()
			if err := c.request(true); err != nil {
				return c.ended(ctx, err)
			}
		case msgColourMap:
			var m struct {
				Pad          byte
				First, Count uint16
			}
			if err := binary.Read(c.r, binary.BigEndian, &m); err != nil {
				return c.ended(ctx, err)
			}
			if _, err := c.r.Discard(int(m.Count) * 6); err != nil {
				return c.ended(ctx, err)
			}
		case msgBell:
		case msgCutText:
			var m struct {
				Pad    [3]byte
				Length uint32
			}
			if err := binary.Read(c.r, binary.BigEndian, &m); err != nil {
				return c.ended(ctx, err)
			}
			if _, err := c.r.Discard(int(m.Length)); err != nil {
				return c.ended(ctx, err)
			}
		default:
			return fmt.Errorf("vnc: unknown message %d", kind)
		}
	}
}

func (c *Client) ended(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// update applies one FramebufferUpdate's rectangles to Screen.
func (c *Client) update() error {
	var head struct {
		Pad   byte
		Count uint16
	}
	if err := binary.Read(c.r, binary.BigEndian, &head); err != nil {
		return err
	}
	for range head.Count {
		var rect struct {
			X, Y, W, H uint16
			Encoding   int32
		}
		if err := binary.Read(c.r, binary.BigEndian, &rect); err != nil {
			return err
		}
		x, y, w, h := int(rect.X), int(rect.Y), int(rect.W), int(rect.H)
		switch rect.Encoding {
		case encodingRaw:
			if x+w > c.Width || y+h > c.Height {
				return fmt.Errorf("vnc: a %dx%d update at %d,%d is off the screen", w, h, x, y)
			}
			for row := range h {
				start := ((y+row)*c.Width + x) * 4
				if _, err := io.ReadFull(c.r, c.Screen[start:start+w*4]); err != nil {
					return err
				}
			}
		case encodingCopy:
			var src struct{ X, Y uint16 }
			if err := binary.Read(c.r, binary.BigEndian, &src); err != nil {
				return err
			}
			c.copyRect(int(src.X), int(src.Y), x, y, w, h)
		case encodingResize:
			c.resize(w, h)
		default:
			return fmt.Errorf("vnc: unrequested encoding %d", rect.Encoding)
		}
	}
	return nil
}

func (c *Client) copyRect(sx, sy, x, y, w, h int) {
	block := make([]byte, w*h*4)
	for row := range h {
		from := ((sy+row)*c.Width + sx) * 4
		copy(block[row*w*4:(row+1)*w*4], c.Screen[from:from+w*4])
	}
	for row := range h {
		to := ((y+row)*c.Width + x) * 4
		copy(c.Screen[to:to+w*4], block[row*w*4:(row+1)*w*4])
	}
}

func (c *Client) resize(w, h int) {
	c.size.Lock()
	c.Width, c.Height = w, h
	c.size.Unlock()
	c.Screen = make([]byte, w*h*4)
}

func (c *Client) request(incremental bool) error {
	m := []byte{msgRequest, 0}
	if incremental {
		m[1] = 1
	}
	m = binary.BigEndian.AppendUint16(m, 0)
	m = binary.BigEndian.AppendUint16(m, 0)
	m = binary.BigEndian.AppendUint16(m, uint16(c.Width))
	m = binary.BigEndian.AppendUint16(m, uint16(c.Height))
	return c.write(m)
}

// Pointer moves the pointer to x, y in screen pixels with buttons held (bit
// 0 the left button, 1 the middle, 2 the right).
func (c *Client) Pointer(x, y int, buttons byte) error {
	m := []byte{msgPointer, buttons}
	m = binary.BigEndian.AppendUint16(m, uint16(max(x, 0)))
	m = binary.BigEndian.AppendUint16(m, uint16(max(y, 0)))
	return c.write(m)
}

// Key presses or releases the key keysym names (an X11 keysym; Latin-1
// characters are their own).
func (c *Client) Key(keysym uint32, down bool) error {
	m := []byte{msgKey, 0, 0, 0}
	if down {
		m[1] = 1
	}
	return c.write(binary.BigEndian.AppendUint32(m, keysym))
}

func (c *Client) write(b []byte) error {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	_, err := c.conn.Write(b)
	return err
}

func (c *Client) reason() error {
	var n uint32
	if err := binary.Read(c.r, binary.BigEndian, &n); err != nil {
		return err
	}
	reason := make([]byte, min(n, 1024))
	io.ReadFull(c.r, reason)
	return fmt.Errorf("vnc: %s", reason)
}

func contains(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}
