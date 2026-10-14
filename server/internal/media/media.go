// Package media validates and stores uploaded images on the local filesystem.
package media

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	"io"
	"os"
	"path/filepath"

	// These blank imports register the decoders image.Decode dispatches to, and they are the whole
	// reason this package exists in its own file: without them every upload would be refused as an
	// unknown format. WebP comes from x/image because the standard library has no WebP decoder.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

var (
	ErrTooLarge    = errors.New("image exceeds the byte limit")
	ErrUnsupported = errors.New("unsupported or corrupt image")
	ErrTooWide     = errors.New("image exceeds the dimension limit")
	ErrEmpty       = errors.New("image is empty")
)

// mimeTypes maps the format that was actually decoded to the type stored with the row. A declared
// Content-Type is never trusted: the bytes decide what the file is.
var mimeTypes = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
}

// Limits are the administrator-configured bounds for one image.
type Limits struct {
	MaxBytes     int64
	MaxDimension int
}

// Stored describes one accepted image.
type Stored struct {
	Key      string
	MIMEType string
	ByteSize int64
	Width    int
	Height   int
}

// Store writes image files under a single directory.
type Store struct{ Root string }

// Save validates one image and writes it under a fresh random key.
//
// The path is derived entirely from the generated key, never from anything the client sent, so a crafted
// filename cannot escape the directory or overwrite another image.
func (s Store) Save(r io.Reader, limits Limits) (Stored, error) {
	var stored Stored
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = 5 << 20
	}
	if limits.MaxDimension <= 0 {
		limits.MaxDimension = 4096
	}
	// Read one byte past the limit so an over-sized body is detected rather than silently truncated.
	data, err := io.ReadAll(io.LimitReader(r, limits.MaxBytes+1))
	if err != nil {
		return stored, err
	}
	if int64(len(data)) > limits.MaxBytes {
		return stored, ErrTooLarge
	}
	if len(data) == 0 {
		return stored, ErrEmpty
	}
	format, width, height, err := inspect(data)
	if err != nil {
		return stored, err
	}
	mimeType, ok := mimeTypes[format]
	if !ok {
		// A format the decoder knows but the product does not accept, for example BMP.
		return stored, ErrUnsupported
	}
	if width > limits.MaxDimension || height > limits.MaxDimension {
		return stored, ErrTooWide
	}
	if width <= 0 || height <= 0 {
		return stored, ErrUnsupported
	}
	key, err := randomKey()
	if err != nil {
		return stored, err
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return stored, err
	}
	if err := os.WriteFile(filepath.Join(s.Root, key), data, 0o644); err != nil {
		return stored, err
	}
	return Stored{Key: key, MIMEType: mimeType, ByteSize: int64(len(data)), Width: width, Height: height}, nil
}

// inspect decodes the header and then the pixels. Decoding the pixels too, rather than trusting the
// header, means a truncated or corrupt file is refused at upload instead of being stored and later
// failing to render for every reader.
func inspect(data []byte) (string, int, int, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, ErrUnsupported
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return "", 0, 0, ErrUnsupported
	}
	return format, config.Width, config.Height, nil
}

// randomKey is the storage key. Random rather than derived from the upload means the URL reveals nothing
// about the uploader, the original filename, or the order of uploads.
func randomKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// Open reads a stored image back. It refuses anything that is not a plain file inside the root, so a key
// containing path separators cannot be used to read neighbouring files.
func (s Store) Open(key string) (*os.File, error) {
	if !validKey(key) {
		return nil, ErrUnsupported
	}
	file, err := os.Open(filepath.Join(s.Root, key))
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrUnsupported
	}
	return file, nil
}

// Remove deletes a stored file. It is used to undo a write whose database row failed, so that no file is
// left behind with no row describing it — such a file is invisible to every cleanup that works from the
// database.
func (s Store) Remove(key string) error {
	if !validKey(key) {
		return ErrUnsupported
	}
	err := os.Remove(filepath.Join(s.Root, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// validKey accepts exactly the shape randomKey produces.
func validKey(key string) bool {
	if len(key) != 32 {
		return false
	}
	for _, r := range key {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}
