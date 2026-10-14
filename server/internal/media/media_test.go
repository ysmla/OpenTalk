package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/HugoSmits86/nativewebp"
)

// testLimits are wide enough that the validation limits never interfere with a metadata test.
func testLimits() Limits { return Limits{MaxBytes: 1 << 20, MaxDimension: 512} }

// save stores one payload in a throwaway directory and returns the stored bytes.
func save(t *testing.T, payload []byte) []byte {
	t.Helper()
	return saveWithLimits(t, payload, testLimits())
}

// saveWithLimits is the form used by the limit tests, which need their own bounds.
func saveWithLimits(t *testing.T, payload []byte, limits Limits) []byte {
	t.Helper()
	store := Store{Root: t.TempDir()}
	stored, err := store.Save(bytes.NewReader(payload), limits)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(store.Root, stored.Key))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// gradient builds a small deterministic image whose pixels differ in both axes, so a rotation or a
// mirror is visible rather than being hidden by a uniform colour.
func gradient(width, height int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(20 + x*7), G: uint8(40 + y*11), B: uint8(60 + x + y), A: 255})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func encodeWebP(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := nativewebp.Encode(&buffer, img, nil); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func encodeGIF(t *testing.T, frames []*image.Paletted, delays []int) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := gif.EncodeAll(&buffer, &gif.GIF{Image: frames, Delay: delays, LoopCount: 0}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// pngChunk builds a PNG chunk with a valid CRC, so an injected text chunk is a legal part of the file
// rather than something a decoder would reject.
func pngChunk(kind string, payload []byte) []byte {
	chunk := make([]byte, 0, len(payload)+12)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	chunk = append(chunk, length[:]...)
	chunk = append(chunk, kind...)
	chunk = append(chunk, payload...)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(chunk[4:]))
	return append(chunk, checksum[:]...)
}

// pngWithText inserts a tEXt chunk directly after IHDR, which is where a camera or an editor writes the
// author, device and location fields.
func pngWithText(t *testing.T, img image.Image, keyword, marker string) []byte {
	t.Helper()
	original := encodePNG(t, img)
	const header = 8
	const ihdrLength = 25
	text := pngChunk("tEXt", []byte(keyword+"\x00"+marker))
	withText := make([]byte, 0, len(original)+len(text))
	withText = append(withText, original[:header+ihdrLength]...)
	withText = append(withText, text...)
	withText = append(withText, original[header+ihdrLength:]...)
	return withText
}

// jpegWithComment inserts a COM segment after SOI. A COM segment is legal EXIF-adjacent metadata, and it
// is exactly what the review probe used to show that the stored file kept its private marker.
func jpegWithComment(t *testing.T, img image.Image, marker string) []byte {
	t.Helper()
	original := encodeJPEG(t, img)
	segment := []byte{0xff, 0xfe, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(marker)+2))
	withComment := append([]byte{}, original[:2]...)
	withComment = append(withComment, segment...)
	withComment = append(withComment, marker...)
	withComment = append(withComment, original[2:]...)
	return withComment
}

// gifWithComment inserts a comment extension block before the first frame. This is where an editor writes
// the tool name and, in practice, arbitrary text.
func gifWithComment(t *testing.T, frames []*image.Paletted, delays []int, marker string) []byte {
	t.Helper()
	original := encodeGIF(t, frames, delays)
	if len(original) < 13 {
		t.Fatal("encoded GIF is too short to carry a comment")
	}
	flags := original[10]
	offset := 13
	if flags&0x80 != 0 {
		// The global colour table follows the logical screen descriptor when its flag is set.
		offset += 3 * (1 << ((flags & 0x07) + 1))
	}
	if offset > len(original) {
		t.Fatal("encoded GIF has a colour table beyond its own length")
	}
	comment := []byte{0x21, 0xfe}
	for start := 0; start < len(marker); start += 255 {
		end := start + 255
		if end > len(marker) {
			end = len(marker)
		}
		block := marker[start:end]
		comment = append(comment, byte(len(block)))
		comment = append(comment, block...)
	}
	comment = append(comment, 0x00)
	withComment := make([]byte, 0, len(original)+len(comment))
	withComment = append(withComment, original[:offset]...)
	withComment = append(withComment, comment...)
	withComment = append(withComment, original[offset:]...)
	return withComment
}

// tiffBytes builds a minimal little-endian TIFF directory holding only the orientation tag.
func tiffBytes(orientation uint16) []byte {
	blob := make([]byte, 26)
	copy(blob[0:2], "II")
	binary.LittleEndian.PutUint16(blob[2:4], 42)
	binary.LittleEndian.PutUint32(blob[4:8], 8)
	binary.LittleEndian.PutUint16(blob[8:10], 1)
	binary.LittleEndian.PutUint16(blob[10:12], 0x0112)
	binary.LittleEndian.PutUint16(blob[12:14], 3)
	binary.LittleEndian.PutUint32(blob[14:18], 1)
	binary.LittleEndian.PutUint16(blob[18:20], orientation)
	binary.LittleEndian.PutUint32(blob[22:26], 0)
	return blob
}

// jpegWithOrientation writes a real APP1/Exif segment carrying one orientation value.
func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	original := encodeJPEG(t, img)
	tiff := tiffBytes(orientation)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	withExif := append([]byte{}, original[:2]...)
	withExif = append(withExif, segment...)
	withExif = append(withExif, payload...)
	withExif = append(withExif, original[2:]...)
	return withExif
}

// declaredSize decodes only the header, which is what a reader sees before any pixel is touched.
func declaredSize(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("stored bytes are not a decodable image: %v", err)
	}
	return config.Width, config.Height, format
}

// TestUploadStripsImageMetadata is the regression for the review finding that uploads were written to
// disk exactly as they arrived, carrying GPS and device metadata written by the camera.
func TestUploadStripsImageMetadata(t *testing.T) {
	const marker = "private-location-and-camera-review-marker"
	img := gradient(12, 9)

	t.Run("JPEG的COM段被剥离", func(t *testing.T) {
		payload := jpegWithComment(t, img, marker)
		if !bytes.Contains(payload, []byte(marker)) {
			t.Fatal("the fixture does not contain the metadata it is meant to prove is removed")
		}
		stored := save(t, payload)
		if bytes.Contains(stored, []byte(marker)) {
			t.Fatal("privacy violation: uploaded JPEG metadata was stored unchanged")
		}
		if _, _, format := declaredSize(t, stored); format != "jpeg" {
			t.Fatalf("a JPEG must stay a JPEG: %s", format)
		}
	})

	t.Run("PNG的tEXt文本块被剥离", func(t *testing.T) {
		payload := pngWithText(t, img, "Comment", marker)
		if !bytes.Contains(payload, []byte(marker)) {
			t.Fatal("the fixture does not contain the metadata it is meant to prove is removed")
		}
		stored := save(t, payload)
		if bytes.Contains(stored, []byte(marker)) {
			t.Fatal("privacy violation: uploaded PNG metadata was stored unchanged")
		}
		if _, _, format := declaredSize(t, stored); format != "png" {
			t.Fatalf("a PNG must stay a PNG: %s", format)
		}
	})

	t.Run("GIF的注释扩展块被剥离且动画保留", func(t *testing.T) {
		palette := color.Palette{color.NRGBA{R: 255, A: 255}, color.NRGBA{B: 255, A: 255}}
		frames := []*image.Paletted{
			image.NewPaletted(image.Rect(0, 0, 4, 4), palette),
			image.NewPaletted(image.Rect(0, 0, 4, 4), palette),
		}
		// The second frame is filled so the two frames are genuinely different pictures.
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				frames[1].SetColorIndex(x, y, 1)
			}
		}
		payload := gifWithComment(t, frames, []int{10, 20}, marker)
		if !bytes.Contains(payload, []byte(marker)) {
			t.Fatal("the fixture does not contain the metadata it is meant to prove is removed")
		}
		stored := save(t, payload)
		if bytes.Contains(stored, []byte(marker)) {
			t.Fatal("privacy violation: uploaded GIF metadata was stored unchanged")
		}
		animated, err := gif.DecodeAll(bytes.NewReader(stored))
		if err != nil {
			t.Fatalf("stored GIF is not decodable: %v", err)
		}
		if len(animated.Image) != 2 {
			t.Fatalf("re-encoding must preserve the animation, got %d frame(s)", len(animated.Image))
		}
		if len(animated.Delay) != 2 || animated.Delay[0] != 10 || animated.Delay[1] != 20 {
			t.Fatalf("frame delays changed: %v", animated.Delay)
		}
	})

	t.Run("WebP被重新编码且不残留原字节", func(t *testing.T) {
		payload := encodeWebP(t, img)
		stored := save(t, payload)
		if _, _, format := declaredSize(t, stored); format != "webp" {
			t.Fatalf("a WebP must stay a WebP: %s", format)
		}
		if !samePixels(t, decodeStored(t, stored), img) {
			t.Fatal("the re-encoded WebP does not show the uploaded picture")
		}
	})

	t.Run("所有格式的图片内容都保持不变", func(t *testing.T) {
		// Removing metadata must not damage the picture: this is the guard against a "privacy fix" that
		// silently re-encodes everything into a blank or truncated image.
		cases := []struct {
			name    string
			payload []byte
		}{
			{"png", encodePNG(t, img)},
			{"webp", encodeWebP(t, img)},
		}
		for _, item := range cases {
			stored := save(t, item.payload)
			width, height, _ := declaredSize(t, stored)
			if width != 12 || height != 9 {
				t.Fatalf("%s changed dimensions to %dx%d", item.name, width, height)
			}
			if !samePixels(t, decodeStored(t, stored), img) {
				t.Fatalf("%s lost pixel fidelity", item.name)
			}
		}
		// JPEG is lossy, so it is compared with a tolerance instead of exactly.
		stored := save(t, encodeJPEG(t, img))
		width, height, _ := declaredSize(t, stored)
		if width != 12 || height != 9 {
			t.Fatalf("jpeg changed dimensions to %dx%d", width, height)
		}
		if !closePixels(t, decodeStored(t, stored), img, 12) {
			t.Fatal("jpeg lost pixel fidelity beyond the lossy tolerance")
		}
	})
}

// TestUploadAppliesExifOrientation proves that stripping the orientation tag does not rotate the picture:
// the pixels have to absorb the rotation the tag described.
func TestUploadAppliesExifOrientation(t *testing.T) {
	img := gradient(16, 8)

	t.Run("方向1与无标记都不旋转", func(t *testing.T) {
		for _, payload := range [][]byte{encodeJPEG(t, img), jpegWithOrientation(t, img, 1)} {
			stored := save(t, payload)
			width, height, _ := declaredSize(t, stored)
			if width != 16 || height != 8 {
				t.Fatalf("an upright image must keep its orientation: %dx%d", width, height)
			}
		}
	})

	t.Run("方向6旋转90度并交换宽高", func(t *testing.T) {
		stored := save(t, jpegWithOrientation(t, img, 6))
		width, height, _ := declaredSize(t, stored)
		if width != 8 || height != 16 {
			t.Fatalf("orientation 6 must transpose the image, got %dx%d", width, height)
		}
		// The top-left pixel of the rotated result comes from the bottom-left of the original.
		decoded := decodeStored(t, stored)
		want := img.NRGBAAt(0, 7)
		got := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
		if !closeTo(got.R, want.R, 12) || !closeTo(got.G, want.G, 12) || !closeTo(got.B, want.B, 12) {
			t.Fatalf("orientation 6 rotated the wrong corner: got %v want %v", got, want)
		}
	})

	t.Run("方向3旋转180度", func(t *testing.T) {
		stored := save(t, jpegWithOrientation(t, img, 3))
		width, height, _ := declaredSize(t, stored)
		if width != 16 || height != 8 {
			t.Fatalf("orientation 3 must keep the axes, got %dx%d", width, height)
		}
		decoded := decodeStored(t, stored)
		want := img.NRGBAAt(15, 7)
		got := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
		if !closeTo(got.R, want.R, 12) || !closeTo(got.G, want.G, 12) || !closeTo(got.B, want.B, 12) {
			t.Fatalf("orientation 3 rotated the wrong corner: got %v want %v", got, want)
		}
	})

	t.Run("超出1..8的方向值按不旋转处理", func(t *testing.T) {
		stored := save(t, jpegWithOrientation(t, img, 9))
		width, height, _ := declaredSize(t, stored)
		if width != 16 || height != 8 {
			t.Fatalf("an unknown orientation must be ignored, got %dx%d", width, height)
		}
	})
}

// TestSaveLimitsBeforeDecodingPixels covers the ordering requirement: the declared dimensions must be
// checked from the header, so an over-sized image is refused without a pixel decode.
func TestSaveLimitsBeforeDecodingPixels(t *testing.T) {
	// A truncated file is the cheapest proof that the dimension check runs first: it has a valid header
	// declaring 600 pixels wide and no complete pixel data at all. Reaching the dimension limit proves the
	// header check decided the outcome, and reaching the pixel decode would have produced the same
	// generic error instead.
	wide := gradient(600, 4)
	payload := encodePNG(t, wide)
	header := payload[:len(payload)/2]
	store := Store{Root: t.TempDir()}
	_, err := store.Save(bytes.NewReader(header), Limits{MaxBytes: 1 << 20, MaxDimension: 100})
	if err != ErrTooWide {
		t.Fatalf("a header over the dimension limit must be refused as too wide, got %v", err)
	}
	if _, err := store.Save(bytes.NewReader(payload), Limits{MaxBytes: 1 << 20, MaxDimension: 100}); err != ErrTooWide {
		t.Fatalf("a complete over-wide image must also be refused, got %v", err)
	}
	if _, err := store.Save(bytes.NewReader(payload), Limits{MaxBytes: 1 << 20, MaxDimension: 600}); err != nil {
		t.Fatalf("the same image must be accepted when the limit allows it: %v", err)
	}
}

// TestSaveRefusesUnsupportedAndEmpty keeps the existing refusal behaviour covered after the re-encode was
// introduced.
func TestSaveRefusesUnsupportedAndEmpty(t *testing.T) {
	store := Store{Root: t.TempDir()}
	if _, err := store.Save(bytes.NewReader(nil), testLimits()); err != ErrEmpty {
		t.Fatalf("an empty upload must be ErrEmpty, got %v", err)
	}
	if _, err := store.Save(bytes.NewReader([]byte("this is not an image")), testLimits()); err != ErrUnsupported {
		t.Fatalf("a non-image must be ErrUnsupported, got %v", err)
	}
	// A truncated PNG has a readable header but no usable pixels, so it must be refused rather than stored
	// as a file that fails to render for every reader.
	full := encodePNG(t, gradient(32, 32))
	if _, err := store.Save(bytes.NewReader(full[:len(full)-8]), testLimits()); err != ErrUnsupported {
		t.Fatalf("a corrupt image must be ErrUnsupported, got %v", err)
	}
	if _, err := store.Save(bytes.NewReader(make([]byte, testLimits().MaxBytes+1)), testLimits()); err != ErrTooLarge {
		t.Fatalf("an over-sized upload must be ErrTooLarge, got %v", err)
	}
}

func decodeStored(t *testing.T, data []byte) image.Image {
	t.Helper()
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("stored bytes are not decodable: %v", err)
	}
	return decoded
}

func samePixels(t *testing.T, left, right image.Image) bool {
	t.Helper()
	if left.Bounds() != right.Bounds() {
		return false
	}
	for y := left.Bounds().Min.Y; y < left.Bounds().Max.Y; y++ {
		for x := left.Bounds().Min.X; x < left.Bounds().Max.X; x++ {
			lr, lg, lb, la := left.At(x, y).RGBA()
			rr, rg, rb, ra := right.At(x, y).RGBA()
			if lr != rr || lg != rg || lb != rb || la != ra {
				return false
			}
		}
	}
	return true
}

func closePixels(t *testing.T, got, want image.Image, tolerance int) bool {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		return false
	}
	for y := got.Bounds().Min.Y; y < got.Bounds().Max.Y; y++ {
		for x := got.Bounds().Min.X; x < got.Bounds().Max.X; x++ {
			gr, gg, gb, _ := got.At(x, y).RGBA()
			wr, wg, wb, _ := want.At(x, y).RGBA()
			if !closeTo(uint8(gr>>8), uint8(wr>>8), tolerance) ||
				!closeTo(uint8(gg>>8), uint8(wg>>8), tolerance) ||
				!closeTo(uint8(gb>>8), uint8(wb>>8), tolerance) {
				return false
			}
		}
	}
	return true
}

func closeTo(got, want uint8, tolerance int) bool {
	delta := int(got) - int(want)
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}
