// Package captcha renders self-contained image challenges without any
// third-party dependency: the project keeps its dependency list tiny, and a
// captcha renderer never justifies growing it.
package captcha

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Alphabet is the character set used for challenges. It deliberately omits
// visually confusable characters: 0, O, 1, I and L.
const Alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

const (
	canvasWidth  = 250
	canvasHeight = 70

	// glyphScale keeps characters large enough to read at the maximum challenge
	// length while still leaving room for the per-character distortion to move
	// them without touching a neighbour or clipping at the canvas edge.
	glyphScale = 5

	// glyphW and glyphH are the logical bitmap dimensions. Every glyph is a
	// 7-row table where each row holds 5 bits, most significant bit leftmost.
	glyphW = 5
	glyphH = 7

	// sampleGrid supersamples each output pixel, which antialiases the rotated
	// and sheared glyph edges that a plain pixel test would leave jagged.
	sampleGrid = 4

	maxRunes = 8
)

var (
	background = color.NRGBA{R: 0xF7, G: 0xF7, B: 0xF2, A: 0xFF}
	ink        = color.NRGBA{R: 0x1A, G: 0x1C, B: 0x21, A: 0xFF}

	// Noise colours stay in a mid-tone band: distinct from the near-black text,
	// but light enough that a stray line over a glyph cannot hide it.
	noiseLineColors = []color.NRGBA{
		{R: 0x9E, G: 0xC5, B: 0xE8, A: 0xFF},
		{R: 0xF2, G: 0xC3, B: 0xB6, A: 0xFF},
		{R: 0xBF, G: 0xE0, B: 0xC4, A: 0xFF},
	}
	noiseDotColors = []color.NRGBA{
		{R: 0x7C, G: 0x9F, B: 0xC4, A: 0xFF},
		{R: 0xD8, G: 0x9A, B: 0x8C, A: 0xFF},
		{R: 0x93, G: 0xBF, B: 0x9A, A: 0xFF},
	}
)

// glyphs maps each supported rune to 7 rows of 5 bits. The tables are hand
// written so the shapes stay recognisable once rotation and shear are applied.
var glyphs = map[rune][glyphH]uint8{
	'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
	'C': {0b01110, 0b10001, 0b10000, 0b10000, 0b10000, 0b10001, 0b01110},
	'D': {0b11110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11110},
	'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
	'F': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b10000},
	'G': {0b01110, 0b10001, 0b10000, 0b10111, 0b10001, 0b10001, 0b01111},
	'H': {0b10001, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'J': {0b00111, 0b00010, 0b00010, 0b00010, 0b00010, 0b10010, 0b01100},
	'K': {0b10001, 0b10010, 0b10100, 0b11000, 0b10100, 0b10010, 0b10001},
	'M': {0b10001, 0b11011, 0b10101, 0b10101, 0b10001, 0b10001, 0b10001},
	'N': {0b10001, 0b11001, 0b10101, 0b10011, 0b10001, 0b10001, 0b10001},
	'P': {0b11110, 0b10001, 0b10001, 0b11110, 0b10000, 0b10000, 0b10000},
	'Q': {0b01110, 0b10001, 0b10001, 0b10001, 0b10101, 0b10010, 0b01101},
	'R': {0b11110, 0b10001, 0b10001, 0b11110, 0b10100, 0b10010, 0b10001},
	'S': {0b01111, 0b10000, 0b10000, 0b01110, 0b00001, 0b00001, 0b11110},
	'T': {0b11111, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100},
	'U': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'V': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01010, 0b00100},
	'W': {0b10001, 0b10001, 0b10001, 0b10101, 0b10101, 0b11011, 0b10001},
	'X': {0b10001, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001, 0b10001},
	'Y': {0b10001, 0b10001, 0b01010, 0b00100, 0b00100, 0b00100, 0b00100},
	'Z': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b10000, 0b11111},
	'2': {0b01110, 0b10001, 0b00001, 0b00110, 0b01000, 0b10000, 0b11111},
	'3': {0b11111, 0b00010, 0b00100, 0b01110, 0b00001, 0b10001, 0b01110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	'6': {0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100},
}

// RenderPNG draws text as a distorted PNG challenge image. Rendering is
// deterministic: the same text and seed always produce identical bytes.
func RenderPNG(text string, seed int64) ([]byte, error) {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil, errors.New("captcha: text must not be empty")
	}
	if len(runes) > maxRunes {
		return nil, fmt.Errorf("captcha: text is %d runes, maximum is %d", len(runes), maxRunes)
	}
	for _, ch := range runes {
		if _, ok := glyphs[ch]; !ok {
			return nil, fmt.Errorf("captcha: %q is not in Alphabet", ch)
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	fill(img, background)

	// Seeding from the caller's value only keeps concurrent renders independent
	// of process-wide math/rand state and of each other.
	r := newRand(seed)
	drawNoise(img, r)
	drawText(img, runes, r)

	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("captcha: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// drawText centres the glyph row horizontally and places every character with
// its own offset, rotation and shear so no two positions look identical.
func drawText(img *image.NRGBA, runes []rune, r *rand) {
	cellW := float64(glyphW * glyphScale)
	gap := 6.0
	total := float64(len(runes))*cellW + float64(len(runes)-1)*gap
	x := (float64(canvasWidth) - total) / 2
	baseY := (float64(canvasHeight) - float64(glyphH*glyphScale)) / 2

	for _, ch := range runes {
		dx := r.float64()*2.6 - 1.3
		dy := r.float64()*6 - 3
		angle := (r.float64()*2 - 1) * 0.22 // radians, about +-13 degrees
		shear := (r.float64()*2 - 1) * 0.10 // horizontal lean
		drawGlyph(img, glyphs[ch], x+dx, baseY+dy, angle, shear)
		x += cellW + gap
	}
}

// drawGlyph rasterises one glyph through an affine transform. Sampling runs
// over the transformed bounding box instead of the source box, so rotation and
// shear grow the ink rather than clipping it.
func drawGlyph(dst *image.NRGBA, glyph [glyphH]uint8, x, y, angle, shear float64) {
	scale := float64(glyphScale)
	cos, sin := math.Cos(angle), math.Sin(angle)

	// forward: (u,v) -> rotate, shear, scale, translate.
	a, b := cos*scale, sin*scale
	c, d := (-sin+cos*shear)*scale, (cos+sin*shear)*scale

	det := a*d - b*c
	if det == 0 {
		return // degenerate transform, nothing meaningful to draw
	}

	// Corner extents give the axis-aligned destination box.
	minX, maxX := math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, corner := range [4][2]float64{{0, 0}, {glyphW, 0}, {0, glyphH}, {glyphW, glyphH}} {
		px := a*corner[0] + b*corner[1] + x
		py := c*corner[0] + d*corner[1] + y
		minX, maxX = math.Min(minX, px), math.Max(maxX, px)
		minY, maxY = math.Min(minY, py), math.Max(maxY, py)
	}

	x0, x1 := clampInt(int(math.Floor(minX))-1, 0, canvasWidth-1), clampInt(int(math.Ceil(maxX))+1, 0, canvasWidth-1)
	y0, y1 := clampInt(int(math.Floor(minY))-1, 0, canvasHeight-1), clampInt(int(math.Ceil(maxY))+1, 0, canvasHeight-1)

	step := 1.0 / sampleGrid
	samples := float64(sampleGrid * sampleGrid)

	for py := y0; py <= y1; py++ {
		for px := x0; px <= x1; px++ {
			inside := 0
			for sy := 0; sy < sampleGrid; sy++ {
				for sx := 0; sx < sampleGrid; sx++ {
					dx := float64(px) + (float64(sx)+0.5)*step - x
					dy := float64(py) + (float64(sy)+0.5)*step - y
					u := (d*dx - b*dy) / det
					v := (-c*dx + a*dy) / det
					if bitAt(glyph, int(math.Floor(u)), int(math.Floor(v))) {
						inside++
					}
				}
			}
			if inside > 0 {
				blend(dst, px, py, ink, float64(inside)/samples)
			}
		}
	}
}

// bitAt reports whether the glyph has a set pixel at a local coordinate. Rows
// store the leftmost pixel in the most significant of the five used bits.
func bitAt(glyph [glyphH]uint8, x, y int) bool {
	if x < 0 || y < 0 || x >= glyphW || y >= glyphH {
		return false
	}
	return glyph[y]&(1<<uint(glyphW-1-x)) != 0
}

// drawNoise scatters thin lines and dots in a colour that cannot be mistaken
// for the text, keeping the challenge readable while defeating a naive
// background-colour threshold.
func drawNoise(img *image.NRGBA, r *rand) {
	lineColor := noiseLineColors[r.intn(len(noiseLineColors))]
	dotColor := noiseDotColors[r.intn(len(noiseDotColors))]

	for i := 0; i < 3; i++ {
		y := r.float64() * canvasHeight
		slope := (r.float64()*2 - 1) * (canvasHeight / 3)
		for x := 0; x < canvasWidth; x++ {
			wy := y + slope*float64(x)/canvasWidth
			plot(img, x, int(math.Round(wy)), lineColor)
			if i%2 == 0 {
				plot(img, x, int(math.Round(wy))+1, lineColor) // every other line is slightly heavier
			}
		}
	}

	for i := 0; i < 70; i++ {
		cx, cy := int(r.float64()*canvasWidth), int(r.float64()*canvasHeight)
		plot(img, cx, cy, dotColor)
		if r.intn(3) == 0 {
			plot(img, cx+1, cy, dotColor)
			plot(img, cx, cy+1, dotColor)
		}
	}
}

func fill(img *image.NRGBA, c color.NRGBA) {
	for y := img.Rect.Min.Y; y < img.Rect.Max.Y; y++ {
		for x := img.Rect.Min.X; x < img.Rect.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

func plot(img *image.NRGBA, x, y int, c color.NRGBA) {
	if x < 0 || y < 0 || x >= canvasWidth || y >= canvasHeight {
		return
	}
	img.SetNRGBA(x, y, c)
}

// blend mixes c into the existing pixel at coverage (0..1].
func blend(img *image.NRGBA, x, y int, c color.NRGBA, coverage float64) {
	if x < 0 || y < 0 || x >= canvasWidth || y >= canvasHeight {
		return
	}
	if coverage >= 1 {
		img.SetNRGBA(x, y, c)
		return
	}
	dst := img.NRGBAAt(x, y)
	mix := func(d, s uint8) uint8 { return uint8(float64(d) + (float64(s)-float64(d))*coverage) }
	img.SetNRGBA(x, y, color.NRGBA{
		R: mix(dst.R, c.R),
		G: mix(dst.G, c.G),
		B: mix(dst.B, c.B),
		A: 0xFF,
	})
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// rand is a small deterministic generator. Rendering must depend on the seed
// alone, so the package never touches the global math/rand source.
type rand struct{ state uint64 }

func newRand(seed int64) *rand {
	h := fnv.New64a()
	var b [8]byte
	v := uint64(seed)
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	h.Write(b[:])
	// An odd state cannot be reached from a zero starting point, which keeps
	// the sequence from degenerating for unlucky seeds.
	return &rand{state: h.Sum64() | 1}
}

func (r *rand) next() uint64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	// High bits of a linear congruential generator are the well-behaved ones.
	return r.state >> 11
}

// float64 returns a value in [0,1).
func (r *rand) float64() float64 {
	const denom = 1 << 53
	return float64(r.next()%denom) / float64(denom)
}

// intn returns a value in [0,n) and tolerates the n<=0 callers do not make.
func (r *rand) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(r.next() % uint64(n))
}
