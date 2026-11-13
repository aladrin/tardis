//go:build openbsd

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const (
	TardisDomainTag  = "TARDIS-V2\x00"
	TardisKeyIDTag   = "TARDIS-KEY-ID\x00"
	LibreSSLCipher   = "-chacha"
	PBKDF2Iterations = "100000"
	KeyHexTotalLen   = 128 // 64 hex chars (256-bit enc secret) + 64 hex chars (256-bit MAC key)
)

var (
	ErrHMACMismatch     = errors.New("HMAC-SHA256 authentication failed: ciphertext tampered or truncated")
	ErrStreamIncomplete = errors.New("cryptographic stream did not complete cleanly")
)

// KeyMaterial holds domain-separated encryption and authentication secrets loaded from a 0400 keyfile.
type KeyMaterial struct {
	Path      string
	EncSecret []byte // 64 ASCII hex bytes passed to LibreSSL via fd:3 (zero NUL/LF truncation risk)
	MACKey    []byte // 32 raw bytes used for domain-bound HMAC-SHA256
	Created   bool
}

// Fingerprint returns a short non-secret identifier suitable for UI/log display.
func (k *KeyMaterial) Fingerprint() string {
	if k == nil || len(k.MACKey) == 0 {
		return "unloaded"
	}
	mac := hmac.New(sha256.New, k.MACKey)
	_, _ = mac.Write([]byte(TardisKeyIDTag))
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:6])
}

// Wipe zeroes sensitive byte slices in memory.
func (k *KeyMaterial) Wipe() {
	if k == nil {
		return
	}
	clear(k.EncSecret)
	clear(k.MACKey)
}

// VerifyLibreSSL confirms /usr/bin/openssl is present and reports as OpenBSD LibreSSL.
func VerifyLibreSSL() (string, error) {
	out, err := exec.Command(OpenSSLBin, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("execute %s version: %w", OpenSSLBin, err)
	}
	ver := strings.TrimSpace(string(out))
	fields := strings.Fields(ver)
	if len(fields) == 0 || fields[0] != "LibreSSL" {
		return ver, fmt.Errorf("tardis: LibreSSL required, got %q", ver)
	}
	return ver, nil
}

// LoadKey reads and validates an existing 0400/0600-restricted Tardis keyfile without following symlinks.
func LoadKey(path string) (*KeyMaterial, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("key path is empty")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EFTYPE) {
			return nil, fmt.Errorf("refusing symlink keyfile %s", path)
		}
		return nil, fmt.Errorf("open keyfile %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("fstat keyfile %s: %w", path, err)
	}
	if (st.Mode & unix.S_IFMT) != unix.S_IFREG {
		return nil, fmt.Errorf("keyfile %s is not a regular file", path)
	}
	if (st.Mode & 0077) != 0 {
		return nil, fmt.Errorf("keyfile %s has insecure permissions %#o (require chmod 0400)", path, st.Mode&0777)
	}

	raw, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return nil, fmt.Errorf("read keyfile %s: %w", path, err)
	}
	trimmed := bytes.TrimRight(raw, "\r\n")
	if len(trimmed) != KeyHexTotalLen {
		return nil, fmt.Errorf("invalid keyfile %s: expected %d hex chars, got %d", path, KeyHexTotalLen, len(trimmed))
	}

	decoded := make([]byte, 64)
	if _, err := hex.Decode(decoded, trimmed); err != nil {
		clear(decoded)
		return nil, fmt.Errorf("invalid hex in keyfile %s: %w", path, err)
	}
	defer clear(decoded)

	// Preserve exact 64 ASCII hex characters for fd:3 / emergency CLI `head -c 64` compatibility.
	encSecret := make([]byte, 64)
	copy(encSecret, trimmed[:64])

	macKey := make([]byte, 32)
	copy(macKey, decoded[32:64])

	return &KeyMaterial{
		Path:      path,
		EncSecret: encSecret,
		MACKey:    macKey,
	}, nil
}

// LoadOrCreateKey loads an existing keyfile or atomically generates a new 0400 keyfile.
func LoadOrCreateKey(path string) (*KeyMaterial, error) {
	km, err := LoadKey(path)
	if err == nil {
		return km, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir key dir %s: %w", dir, err)
	}

	raw := make([]byte, 64)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return nil, fmt.Errorf("read CSPRNG for key generation: %w", err)
	}
	defer clear(raw)

	hexBuf := make([]byte, KeyHexTotalLen+1)
	hex.Encode(hexBuf[:KeyHexTotalLen], raw)
	hexBuf[KeyHexTotalLen] = '\n'
	defer clear(hexBuf)

	fd, openErr := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0400)
	if openErr != nil {
		if errors.Is(openErr, unix.EEXIST) {
			return LoadKey(path)
		}
		return nil, fmt.Errorf("atomic create keyfile %s: %w", path, openErr)
	}

	f := os.NewFile(uintptr(fd), path)
	writeOK := false
	defer func() {
		if !writeOK {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()

	if _, err := f.Write(hexBuf); err != nil {
		return nil, fmt.Errorf("write keyfile %s: %w", path, err)
	}
	if err := unix.Fsync(fd); err != nil {
		return nil, fmt.Errorf("fsync keyfile %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close keyfile %s: %w", path, err)
	}
	writeOK = true

	km, err = LoadKey(path)
	if err != nil {
		return nil, err
	}
	km.Created = true
	return km, nil
}

// NewObjectMAC initializes a domain-separated HMAC-SHA256 bound to a specific S3 object key.
func NewObjectMAC(macKey []byte, objectKey string) hash.Hash {
	mac := hmac.New(sha256.New, macKey)
	_, _ = mac.Write([]byte(TardisDomainTag))
	_, _ = mac.Write([]byte(objectKey))
	_, _ = mac.Write([]byte{0})
	return mac
}

// FormatHMACRecord formats a BSD-style authenticated digest line bound to objectKey.
func FormatHMACRecord(sum []byte, objectKey string) []byte {
	return []byte(fmt.Sprintf("HMAC-SHA256 (%s) = %s\n", objectKey, hex.EncodeToString(sum)))
}

// ParseHMACRecord parses a FormatHMACRecord payload and verifies the embedded objectKey matches expectedObjectKey.
func ParseHMACRecord(raw []byte, expectedObjectKey string) ([]byte, error) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return nil, errors.New("empty HMAC record")
	}
	const prefix = "HMAC-SHA256 ("
	const sep = ") = "
	if !strings.HasPrefix(line, prefix) {
		return nil, fmt.Errorf("malformed HMAC record header: %q", line)
	}
	rest := line[len(prefix):]
	idx := strings.LastIndex(rest, sep)
	if idx <= 0 {
		return nil, fmt.Errorf("malformed HMAC record separator: %q", line)
	}
	recordedKey := rest[:idx]
	hexTag := strings.TrimSpace(rest[idx+len(sep):])
	if expectedObjectKey != "" && recordedKey != expectedObjectKey {
		return nil, fmt.Errorf("HMAC object key mismatch: record=%q expected=%q", recordedKey, expectedObjectKey)
	}
	sum, err := hex.DecodeString(hexTag)
	if err != nil || len(sum) != sha256.Size {
		return nil, fmt.Errorf("invalid HMAC hex digest for %s", expectedObjectKey)
	}
	return sum, nil
}

// EncryptStream streams plaintext through LibreSSL ChaCha20 (PBKDF2) and computes a domain-bound HMAC-SHA256.
type EncryptStream struct {
	cmd         *exec.Cmd
	stdout      io.ReadCloser
	stderr      bytes.Buffer
	mac         hash.Hash
	objectKey   string
	cipherBytes atomic.Int64

	finishOnce sync.Once
	completed  bool
	finishErr  error
}

// NewEncryptStream starts `/usr/bin/openssl enc -chacha -pbkdf2 -iter 100000 -salt -e -pass fd:3`.
func NewEncryptStream(ctx context.Context, km *KeyMaterial, objectKey string, plain io.Reader) (*EncryptStream, error) {
	if km == nil || len(km.EncSecret) != 64 || len(km.MACKey) != 32 {
		return nil, errors.New("invalid key material")
	}
	if strings.TrimSpace(objectKey) == "" {
		return nil, errors.New("empty object key for HMAC binding")
	}

	passR, passW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create fd:3 pipe: %w", err)
	}

	cmd := exec.CommandContext(
		ctx,
		OpenSSLBin,
		"enc",
		LibreSSLCipher,
		"-pbkdf2",
		"-iter", PBKDF2Iterations,
		"-salt",
		"-e",
		"-pass", "fd:3",
	)
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
	cmd.Stdin = plain
	cmd.ExtraFiles = []*os.File{passR} // Child fd:3

	es := &EncryptStream{
		cmd:       cmd,
		mac:       NewObjectMAC(km.MACKey, objectKey),
		objectKey: objectKey,
	}
	cmd.Stderr = &es.stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = passR.Close()
		_ = passW.Close()
		return nil, fmt.Errorf("attach libressl stdout: %w", err)
	}
	es.stdout = stdout

	if err := cmd.Start(); err != nil {
		_ = passR.Close()
		_ = passW.Close()
		return nil, fmt.Errorf("start libressl encrypt: %w", err)
	}
	_ = passR.Close()

	passLine := make([]byte, len(km.EncSecret)+1)
	copy(passLine, km.EncSecret)
	passLine[len(km.EncSecret)] = '\n'
	_, writeErr := passW.Write(passLine)
	clear(passLine)
	closeErr := passW.Close()
	if writeErr != nil || closeErr != nil {
		_ = es.Close()
		return nil, fmt.Errorf("write secret to fd:3: %v / %v", writeErr, closeErr)
	}

	return es, nil
}

func (e *EncryptStream) Read(p []byte) (int, error) {
	n, err := e.stdout.Read(p)
	if n > 0 {
		_, _ = e.mac.Write(p[:n])
		e.cipherBytes.Add(int64(n))
	}
	if err == io.EOF {
		if fErr := e.finalize(false); fErr != nil {
			return n, fErr
		}
		return n, io.EOF
	}
	if err != nil {
		_ = e.finalize(true)
		return n, err
	}
	return n, nil
}

func (e *EncryptStream) finalize(abort bool) error {
	e.finishOnce.Do(func() {
		_ = e.stdout.Close()
		if abort && e.cmd.Process != nil {
			_ = e.cmd.Process.Kill()
		}
		waitErr := e.cmd.Wait()
		if abort {
			if waitErr != nil {
				e.finishErr = waitErr
			} else {
				e.finishErr = ErrStreamIncomplete
			}
			return
		}
		if waitErr != nil {
			e.finishErr = fmt.Errorf("libressl encrypt failed (%s): %w (%s)",
				e.objectKey, waitErr, strings.TrimSpace(e.stderr.String()))
			return
		}
		e.completed = true
	})
	return e.finishErr
}

// CiphertextBytes returns the total encrypted byte count emitted so far.
func (e *EncryptStream) CiphertextBytes() int64 {
	return e.cipherBytes.Load()
}

// SumHMAC returns the raw 32-byte HMAC-SHA256 and formatted `.hmac` record once encryption has completed cleanly.
func (e *EncryptStream) SumHMAC() ([]byte, []byte, error) {
	if !e.completed {
		if e.finishErr != nil {
			return nil, nil, e.finishErr
		}
		return nil, nil, ErrStreamIncomplete
	}
	sum := e.mac.Sum(nil)
	return sum, FormatHMACRecord(sum, e.objectKey), nil
}

// Close aborts or reaps the underlying LibreSSL child process.
func (e *EncryptStream) Close() error {
	return e.finalize(true)
}

type macTrackingReader struct {
	r   io.Reader
	mac hash.Hash
	n   atomic.Int64
}

func (m *macTrackingReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		_, _ = m.mac.Write(p[:n])
		m.n.Add(int64(n))
	}
	return n, err
}

// DecryptStream verifies domain-bound HMAC-SHA256 over ciphertext while streaming plaintext from LibreSSL.
type DecryptStream struct {
	cmd         *exec.Cmd
	stdout      io.ReadCloser
	stderr      bytes.Buffer
	macReader   *macTrackingReader
	expectedMAC []byte
	objectKey   string

	finishOnce sync.Once
	verified   bool
	aborted    bool
	finishErr  error
}

// NewDecryptStream starts `/usr/bin/openssl enc -chacha -pbkdf2 -iter 100000 -salt -d -pass fd:3`.
func NewDecryptStream(ctx context.Context, km *KeyMaterial, objectKey string, expectedMAC []byte, cipher io.Reader) (*DecryptStream, error) {
	if km == nil || len(km.EncSecret) != 64 || len(km.MACKey) != 32 {
		return nil, errors.New("invalid key material")
	}
	if len(expectedMAC) != sha256.Size {
		return nil, fmt.Errorf("invalid expected HMAC length (%d) for %s", len(expectedMAC), objectKey)
	}

	passR, passW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create fd:3 pipe: %w", err)
	}

	mr := &macTrackingReader{
		r:   cipher,
		mac: NewObjectMAC(km.MACKey, objectKey),
	}

	cmd := exec.CommandContext(
		ctx,
		OpenSSLBin,
		"enc",
		LibreSSLCipher,
		"-pbkdf2",
		"-iter", PBKDF2Iterations,
		"-salt",
		"-d",
		"-pass", "fd:3",
	)
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
	cmd.Stdin = mr
	cmd.ExtraFiles = []*os.File{passR} // Child fd:3

	ds := &DecryptStream{
		cmd:         cmd,
		macReader:   mr,
		expectedMAC: bytes.Clone(expectedMAC),
		objectKey:   objectKey,
	}
	cmd.Stderr = &ds.stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = passR.Close()
		_ = passW.Close()
		return nil, fmt.Errorf("attach libressl stdout: %w", err)
	}
	ds.stdout = stdout

	if err := cmd.Start(); err != nil {
		_ = passR.Close()
		_ = passW.Close()
		return nil, fmt.Errorf("start libressl decrypt: %w", err)
	}
	_ = passR.Close()

	passLine := make([]byte, len(km.EncSecret)+1)
	copy(passLine, km.EncSecret)
	passLine[len(km.EncSecret)] = '\n'
	_, writeErr := passW.Write(passLine)
	clear(passLine)
	closeErr := passW.Close()
	if writeErr != nil || closeErr != nil {
		_ = ds.Close()
		return nil, fmt.Errorf("write secret to fd:3: %v / %v", writeErr, closeErr)
	}

	return ds, nil
}

func (d *DecryptStream) Read(p []byte) (int, error) {
	n, err := d.stdout.Read(p)
	if err == io.EOF {
		if fErr := d.completeAndVerify(false); fErr != nil {
			return n, fErr
		}
		return n, io.EOF
	}
	if err != nil {
		_ = d.Close()
		return n, err
	}
	return n, nil
}

func (d *DecryptStream) completeAndVerify(allowEarlyPipeClose bool) error {
	d.finishOnce.Do(func() {
		_ = d.stdout.Close()
		waitErr := d.cmd.Wait()
		if waitErr != nil && !allowEarlyPipeClose {
			d.finishErr = fmt.Errorf("libressl decrypt failed (%s): %w (%s)",
				d.objectKey, waitErr, strings.TrimSpace(d.stderr.String()))
			return
		}
		// Drain any unread tail so full-object HMAC-SHA256 is verified even when `restore -x` exits early.
		if _, err := io.Copy(io.Discard, d.macReader); err != nil {
			d.finishErr = fmt.Errorf("drain ciphertext for HMAC check (%s): %w", d.objectKey, err)
			return
		}
		actual := d.macReader.mac.Sum(nil)
		if !hmac.Equal(actual, d.expectedMAC) {
			d.finishErr = fmt.Errorf("%w (%s)", ErrHMACMismatch, d.objectKey)
			return
		}
		d.verified = true
	})
	if d.aborted {
		return ErrStreamIncomplete
	}
	return d.finishErr
}

// VerifyComplete finalizes decryption (including selective early-exit restores) and enforces full-object HMAC-SHA256.
func (d *DecryptStream) VerifyComplete() error {
	return d.completeAndVerify(true)
}

// CiphertextBytesRead returns how many ciphertext bytes have been read from the source so far.
func (d *DecryptStream) CiphertextBytesRead() int64 {
	return d.macReader.n.Load()
}

// Close aborts an in-flight decryption without draining remaining network ciphertext if not yet verified.
func (d *DecryptStream) Close() error {
	d.finishOnce.Do(func() {
		d.aborted = true
		_ = d.stdout.Close()
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
		_ = d.cmd.Wait()
		d.finishErr = ErrStreamIncomplete
	})
	return d.finishErr
}

// EncryptAndSignBytes encrypts an in-memory buffer via LibreSSL and returns ciphertext plus formatted `.hmac` record.
func EncryptAndSignBytes(ctx context.Context, km *KeyMaterial, objectKey string, plain []byte) ([]byte, []byte, error) {
	enc, err := NewEncryptStream(ctx, km, objectKey, bytes.NewReader(plain))
	if err != nil {
		return nil, nil, err
	}
	defer enc.Close()

	cipher, err := io.ReadAll(enc)
	if err != nil {
		return nil, nil, err
	}
	_, hmacRec, err := enc.SumHMAC()
	if err != nil {
		return nil, nil, err
	}
	return cipher, hmacRec, nil
}

// VerifyAndDecryptBytes verifies `.hmac` authenticity before and after decrypting an in-memory ciphertext buffer.
func VerifyAndDecryptBytes(ctx context.Context, km *KeyMaterial, objectKey string, cipher []byte, hmacRecord []byte) ([]byte, error) {
	expectedMAC, err := ParseHMACRecord(hmacRecord, objectKey)
	if err != nil {
		return nil, err
	}
	actualMAC := NewObjectMAC(km.MACKey, objectKey)
	_, _ = actualMAC.Write(cipher)
	if !hmac.Equal(actualMAC.Sum(nil), expectedMAC) {
		return nil, fmt.Errorf("%w (%s)", ErrHMACMismatch, objectKey)
	}

	dec, err := NewDecryptStream(ctx, km, objectKey, expectedMAC, bytes.NewReader(cipher))
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	plain, err := io.ReadAll(dec)
	if err != nil {
		return nil, err
	}
	if err := dec.VerifyComplete(); err != nil {
		return nil, err
	}
	return plain, nil
}

// SelfTestLibreSSL performs a live LibreSSL fd:3 ChaCha20 + PBKDF2 + HMAC-SHA256 round-trip check.
func SelfTestLibreSSL(ctx context.Context, km *KeyMaterial) (string, error) {
	ver, err := VerifyLibreSSL()
	if err != nil {
		return "", err
	}
	probe := []byte("tardis-openbsd-libressl-selftest-probe")
	const testKey = "_selftest/probe.dat"

	cipher, hmacRec, err := EncryptAndSignBytes(ctx, km, testKey, probe)
	if err != nil {
		return ver, fmt.Errorf("encrypt self-test: %w", err)
	}
	out, err := VerifyAndDecryptBytes(ctx, km, testKey, cipher, hmacRec)
	if err != nil {
		return ver, fmt.Errorf("decrypt self-test: %w", err)
	}
	if !bytes.Equal(out, probe) {
		return ver, errors.New("self-test plaintext mismatch")
	}

	// Confirm bit-flip detection rejects tampered ciphertext.
	tampered := bytes.Clone(cipher)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := VerifyAndDecryptBytes(ctx, km, testKey, tampered, hmacRec); !errors.Is(err, ErrHMACMismatch) {
		return ver, errors.New("self-test failed to reject tampered ciphertext")
	}

	return ver, nil
}
