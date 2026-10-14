// Package media validates and stores uploaded images on the local filesystem.
package media

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"

	// These blank imports register the decoders image.Decode dispatches to, and they are the whole
	// reason this package exists in its own file: without them every upload would be refused as an
	// unknown format. WebP comes from x/image because the standard library has no WebP decoder.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/HugoSmits86/nativewebp"
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

// reencodeOverheadFactor bounds the growth an accepted image may gain from re-encoding.
//
// Storing an accepted image means decoding and re-encoding it, because that is the only way to remove
// the metadata a camera writes into the container (EXIF/GPS, XMP, IPTC, PNG text chunks, ICC). The
// re-encoded file is normally smaller, but a pathological input can grow, so the write is refused
// rather than being allowed to turn a near-limit upload into an unbounded file on disk.
const reencodeOverheadFactor = 1.25

// jpegQuality is the quality used when a JPEG is re-encoded. It is high enough to stay visually
// faithful for photographs while still dropping every metadata segment.
const jpegQuality = 88

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
// The bytes that reach the disk are never the bytes that arrived. Every accepted image is decoded and
// re-encoded, which is what removes the metadata (EXIF/GPS, XMP, PNG text, ICC) that would otherwise
// let an anonymous upload carry the author's location and device. A container the product does not
// accept, or one whose pixels cannot be decoded, is refused instead of being stored.
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
	normalized, format, width, height, err := normalize(data, limits)
	if err != nil {
		return stored, err
	}
	mimeType, ok := mimeTypes[format]
	if !ok {
		// A format the decoder knows but the product does not accept, for example BMP.
		return stored, ErrUnsupported
	}
	key, err := randomKey()
	if err != nil {
		return stored, err
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return stored, err
	}
	if err := writeFile(filepath.Join(s.Root, key), normalized); err != nil {
		return stored, err
	}
	return Stored{Key: key, MIMEType: mimeType, ByteSize: int64(len(normalized)), Width: width, Height: height}, nil
}

// normalize decodes one accepted image and re-encodes it in its own container without metadata.
//
// The two steps are ordered deliberately. image.DecodeConfig reads only the header, so the dimension
// limit is enforced before any pixel buffer is allocated: a decompression bomb is refused on its
// declared size instead of after an expensive decode. Only then are the pixels decoded, which is what
// proves the file is whole rather than merely well-headed.
func normalize(data []byte, limits Limits) ([]byte, string, int, int, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, ErrUnsupported
	}
	if _, ok := mimeTypes[format]; !ok {
		return nil, "", 0, 0, ErrUnsupported
	}
	if config.Width <= 0 || config.Height <= 0 {
		return nil, "", 0, 0, ErrUnsupported
	}
	if config.Width > limits.MaxDimension || config.Height > limits.MaxDimension {
		return nil, "", 0, 0, ErrTooWide
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, ErrUnsupported
	}
	// An EXIF orientation tag is metadata being removed, so the pixels have to absorb its effect first;
	// otherwise stripping metadata would silently rotate every phone photo by 90 degrees.
	oriented, err := applyOrientation(decoded, exifOrientation(data, format))
	if err != nil {
		return nil, "", 0, 0, err
	}
	encoded, err := reencode(data, oriented, format)
	if err != nil {
		return nil, "", 0, 0, err
	}
	if limit := int64(float64(limits.MaxBytes) * reencodeOverheadFactor); int64(len(encoded)) > limit {
		return nil, "", 0, 0, ErrTooLarge
	}
	return encoded, format, config.Width, config.Height, nil
}

// reencode writes the image back out in its own container, using only encoders that emit no ancillary
// metadata, so nothing the uploader embedded survives. The original bytes are still needed for GIF,
// where the animation only exists in the container and not in the single decoded frame.
func reencode(original []byte, decoded image.Image, format string) ([]byte, error) {
	var buffer bytes.Buffer
	switch format {
	case "png":
		if err := png.Encode(&buffer, decoded); err != nil {
			return nil, ErrUnsupported
		}
	case "jpeg":
		if err := jpeg.Encode(&buffer, decoded, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, ErrUnsupported
		}
	case "webp":
		// Lossless VP8L: every WebP input is accepted, and the container stores no metadata.
		if err := nativewebp.Encode(&buffer, decoded, nil); err != nil {
			return nil, ErrUnsupported
		}
	case "gif":
		// A GIF is animated, so re-encoding a single frame would silently drop the animation. The frames
		// are decoded from the original bytes and written back with their delays, disposal methods and
		// loop count. Decoding also bounds the frame count, so a corrupt animation cannot make the
		// re-encoder allocate without limit.
		animated, err := gif.DecodeAll(bytes.NewReader(original))
		if err != nil {
			return nil, ErrUnsupported
		}
		if err := gif.EncodeAll(&buffer, animated); err != nil {
			return nil, ErrUnsupported
		}
	default:
		return nil, ErrUnsupported
	}
	if buffer.Len() == 0 {
		return nil, ErrUnsupported
	}
	return buffer.Bytes(), nil
}

// writeFile creates the file with owner-only permissions and only then widens it. The content is
// complete before the wider mode is applied, and the upload path renames nothing over an existing
// file, so a reader never observes a partially written image.
func writeFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return os.Chmod(path, 0o644)
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

// exifOrientation extracts the TIFF orientation tag, returning a value in 1..8, or 0 when the image
// carries no readable orientation. Every other tag, including the GPS and device blocks this package
// refuses to store, is read past and discarded.
func exifOrientation(data []byte, format string) int {
	switch format {
	case "jpeg":
		segment, ok := jpegExifSegment(data)
		if !ok {
			return 0
		}
		return tiffOrientation(segment)
	case "png":
		segment, ok := pngExifChunk(data)
		if !ok {
			return 0
		}
		return tiffOrientation(segment)
	case "webp":
		// A WebP EXIF chunk is a bare TIFF block, so the same reader applies.
		return tiffOrientation(webpExifPayload(data))
	default:
		// GIF has no standard orientation metadata.
		return 0
	}
}

// jpegExifSegment returns the body of the APP1 segment that carries "Exif\0\0".
func jpegExifSegment(data []byte) ([]byte, bool) {
	const marker = 0xff
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != marker {
			return nil, false
		}
		code := data[offset+1]
		// Fill bytes and standalone markers carry no length field.
		if code == marker {
			offset++
			continue
		}
		if code == 0x01 || (code >= 0xd0 && code <= 0xd9) {
			offset += 2
			continue
		}
		length := int(data[offset+2])<<8 | int(data[offset+3])
		if length < 2 || offset+2+length > len(data) {
			return nil, false
		}
		payload := data[offset+4 : offset+2+length]
		if code == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			return payload[6:], true
		}
		// The header scan is over once image data starts.
		if code == 0xda {
			return nil, false
		}
		offset += 2 + length
	}
	return nil, false
}

// pngExifChunk returns the body of an eXIf chunk, which holds a bare TIFF block.
func pngExifChunk(data []byte) ([]byte, bool) {
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, false
	}
	for offset := 8; offset+12 <= len(data); {
		length := int(data[offset])<<24 | int(data[offset+1])<<16 | int(data[offset+2])<<8 | int(data[offset+3])
		if length < 0 || offset+12+length > len(data) {
			return nil, false
		}
		kind := string(data[offset+4 : offset+8])
		if kind == "eXIf" {
			return data[offset+8 : offset+8+length], true
		}
		if kind == "IDAT" {
			return nil, false
		}
		offset += 12 + length
	}
	return nil, false
}

// webpExifPayload returns the body of the EXIF chunk of a RIFF/WebP container.
func webpExifPayload(data []byte) []byte {
	if len(data) < 12 || !bytes.HasPrefix(data, []byte("RIFF")) || string(data[8:12]) != "WEBP" {
		return nil
	}
	for offset := 12; offset+8 <= len(data); {
		kind := string(data[offset : offset+4])
		size := int(data[offset+4]) | int(data[offset+5])<<8 | int(data[offset+6])<<16 | int(data[offset+7])<<24
		if size < 0 || offset+8+size > len(data) {
			return nil
		}
		if kind == "EXIF" {
			return data[offset+8 : offset+8+size]
		}
		offset += 8 + size + size%2
	}
	return nil
}

// tiffOrientation reads tag 0x0112 from a TIFF header. It understands both byte orders and follows the
// IFD0 offset rather than assuming the directory starts right after the header.
func tiffOrientation(blob []byte) int {
	if len(blob) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch {
	case blob[0] == 'I' && blob[1] == 'I':
		order = binary.LittleEndian
	case blob[0] == 'M' && blob[1] == 'M':
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(blob[2:4]) != 42 {
		return 0
	}
	offset := int(order.Uint32(blob[4:8]))
	if offset < 8 || offset+2 > len(blob) {
		return 0
	}
	count := int(order.Uint16(blob[offset : offset+2]))
	entry := offset + 2
	for index := 0; index < count; index++ {
		if entry+12 > len(blob) {
			return 0
		}
		if order.Uint16(blob[entry:entry+2]) == 0x0112 {
			// Orientation is a SHORT, so the value sits in the first two bytes of the value field.
			value := int(order.Uint16(blob[entry+8 : entry+10]))
			if value >= 1 && value <= 8 {
				return value
			}
			return 0
		}
		entry += 12
	}
	return 0
}

// applyOrientation rotates or mirrors the decoded pixels so that removing the EXIF tag changes nothing
// about how the image looks. Orientation 1 and an absent tag mean the same thing.
func applyOrientation(src image.Image, orientation int) (image.Image, error) {
	if orientation <= 1 || orientation > 8 {
		return src, nil
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return src, nil
	}
	// Orientations 5..8 transpose the axes.
	outWidth, outHeight := width, height
	if orientation >= 5 {
		outWidth, outHeight = height, width
	}
	dst := image.NewNRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var targetX, targetY int
			switch orientation {
			case 2: // mirrored horizontally
				targetX, targetY = width-1-x, y
			case 3: // rotated 180
				targetX, targetY = width-1-x, height-1-y
			case 4: // mirrored vertically
				targetX, targetY = x, height-1-y
			case 5: // mirrored horizontally, then rotated 270 clockwise
				targetX, targetY = y, x
			case 6: // rotated 90 clockwise
				targetX, targetY = height-1-y, x
			case 7: // mirrored horizontally, then rotated 90 clockwise
				targetX, targetY = height-1-y, width-1-x
			case 8: // rotated 270 clockwise
				targetX, targetY = y, width-1-x
			}
			dst.Set(targetX, targetY, src.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return dst, nil
}
