package captcha

import (
	"bytes"
	"image/png"
	"testing"
)

// renderOnce renders and decodes in one step so every test exercises the same
// decode path a caller would.
func renderOnce(t *testing.T, text string, seed int64) ([]byte, int, int) {
	t.Helper()
	data, err := RenderPNG(text, seed)
	if err != nil {
		t.Fatalf("RenderPNG(%q, %d) returned error: %v", text, seed, err)
	}
	if len(data) == 0 {
		t.Fatalf("RenderPNG(%q, %d) returned no bytes", text, seed)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode(%q, %d) failed: %v", text, seed, err)
	}
	b := img.Bounds()
	if b.Dx() < 160 || b.Dx() > 260 {
		t.Errorf("width = %d, want between 160 and 260", b.Dx())
	}
	if b.Dy() < 60 || b.Dy() > 90 {
		t.Errorf("height = %d, want between 60 and 90", b.Dy())
	}
	return data, b.Dx(), b.Dy()
}

func TestRenderPNGProducesDecodableImage(t *testing.T) {
	tests := []struct {
		name string
		text string
		seed int64
	}{
		{name: "single letter", text: "A", seed: 1},
		{name: "typical challenge", text: "K7PMX3", seed: 42},
		{name: "maximum length", text: "ABCDEFGH", seed: -9},
		{name: "digits only", text: "23456789", seed: 0},
		{name: "negative seed", text: "QZ", seed: -1 << 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renderOnce(t, tt.text, tt.seed)
		})
	}
}

func TestRenderPNGIsDeterministic(t *testing.T) {
	tests := []struct {
		name string
		text string
		seed int64
	}{
		{name: "zero seed", text: "ABCD", seed: 0},
		{name: "positive seed", text: "7KTZ", seed: 987654321},
		{name: "negative seed", text: "WXYZ", seed: -12345},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, err := RenderPNG(tt.text, tt.seed)
			if err != nil {
				t.Fatalf("first render failed: %v", err)
			}
			second, err := RenderPNG(tt.text, tt.seed)
			if err != nil {
				t.Fatalf("second render failed: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Fatalf("renders differ for (%q, %d): %d vs %d bytes", tt.text, tt.seed, len(first), len(second))
			}
		})
	}
}

func TestRenderPNGVariesWithSeed(t *testing.T) {
	tests := []struct {
		name string
		text string
		a, b int64
	}{
		{name: "adjacent seeds", text: "K7PMX3", a: 1, b: 2},
		{name: "distant seeds", text: "K7PMX3", a: 7, b: 900000},
		{name: "sign differs", text: "ABCD", a: 5, b: -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, err := RenderPNG(tt.text, tt.a)
			if err != nil {
				t.Fatalf("render with seed %d failed: %v", tt.a, err)
			}
			second, err := RenderPNG(tt.text, tt.b)
			if err != nil {
				t.Fatalf("render with seed %d failed: %v", tt.b, err)
			}
			if bytes.Equal(first, second) {
				t.Fatalf("seeds %d and %d produced identical bytes for %q", tt.a, tt.b, tt.text)
			}
		})
	}
}

func TestEveryAlphabetRuneRenders(t *testing.T) {
	for _, ch := range Alphabet {
		t.Run(string(ch), func(t *testing.T) {
			renderOnce(t, string(ch), int64(ch))
		})
	}
}

func TestRenderPNGRejectsInvalidText(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "blank", text: " "},
		{name: "nine runes", text: "ABCDEFGHJ"},
		{name: "ten runes", text: "ABCDEFGHJK"},
		{name: "nine runes with a bad rune", text: "ABCDEFGH0"},
		{name: "digit zero", text: "0"},
		{name: "letter O", text: "O"},
		{name: "digit one", text: "1"},
		{name: "letter I", text: "I"},
		{name: "letter L", text: "L"},
		{name: "punctuation", text: "!"},
		{name: "lowercase", text: "a"},
		{name: "chinese character", text: "验"},
		{name: "emoji", text: "🙂"},
		{name: "newline", text: "AB\nCD"},
		{name: "null byte", text: "\x00"},
		{name: "invalid utf-8", text: string([]byte{0xff, 0xfe})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("RenderPNG(%q) panicked: %v", tt.text, rec)
				}
			}()
			data, err := RenderPNG(tt.text, 3)
			if err == nil {
				t.Fatalf("RenderPNG(%q) returned no error", tt.text)
			}
			if data != nil {
				t.Fatalf("RenderPNG(%q) returned %d bytes alongside an error", tt.text, len(data))
			}
		})
	}
}

func TestRenderPNGIsNotFlatColour(t *testing.T) {
	tests := []struct {
		name string
		text string
		seed int64
	}{
		{name: "one rune", text: "M", seed: 11},
		{name: "full challenge", text: "K7PMX3", seed: 2024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := RenderPNG(tt.text, tt.seed)
			if err != nil {
				t.Fatalf("RenderPNG(%q, %d) failed: %v", tt.text, tt.seed, err)
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("png.Decode failed: %v", err)
			}
			colors := map[[4]uint32]struct{}{}
			dark := 0
			b := img.Bounds()
			for y := b.Min.Y; y < b.Max.Y; y++ {
				for x := b.Min.X; x < b.Max.X; x++ {
					r, g, bl, a := img.At(x, y).RGBA()
					colors[[4]uint32{r, g, bl, a}] = struct{}{}
					// Luminance below the midpoint means real ink, not noise.
					if r+g+bl < 3*0x8000/2 {
						dark++
					}
				}
			}
			if len(colors) < 2 {
				t.Fatalf("image for %q has %d distinct colours, want more than one", tt.text, len(colors))
			}
			if dark == 0 {
				t.Fatalf("image for %q contains no dark pixels: the renderer drew nothing", tt.text)
			}
		})
	}
}
