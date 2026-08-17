// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package imageresize_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/imageresize"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}))
	return buf.Bytes()
}

// pngHeaderOnly builds a minimal, structurally valid PNG signature + IHDR
// chunk declaring width x height, with no IDAT/IEND — enough for
// image.DecodeConfig (which only needs the header) to report the declared
// dimensions, without needing a real, fully-decodable image of that size.
func pngHeaderOnly(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	ihdrData := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdrData[0:4], width)
	binary.BigEndian.PutUint32(ihdrData[4:8], height)
	ihdrData[8] = 8  // bit depth
	ihdrData[9] = 6  // color type: truecolor with alpha
	ihdrData[10] = 0 // compression
	ihdrData[11] = 0 // filter
	ihdrData[12] = 0 // interlace

	writeChunk(&buf, "IHDR", ihdrData)
	return buf.Bytes()
}

func writeChunk(buf *bytes.Buffer, chunkType string, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	buf.Write(length[:])

	typeAndData := append([]byte(chunkType), data...)
	buf.Write(typeAndData)

	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(typeAndData))
	buf.Write(crc[:])
}

func decodedDimensions(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	return cfg.Width, cfg.Height
}

func TestShrinkToMax_NoOpWhenWithinBounds(t *testing.T) {
	data := encodePNG(t, 500, 300)

	out, err := imageresize.ShrinkToMax(data, "image/png", 1024)

	require.NoError(t, err)
	assert.Equal(t, data, out, "an image already within bounds must be returned unchanged, not re-encoded")
}

func TestShrinkToMax_NoOpAtExactlyMaxDim(t *testing.T) {
	data := encodePNG(t, 1024, 1024)

	out, err := imageresize.ShrinkToMax(data, "image/png", 1024)

	require.NoError(t, err)
	assert.Equal(t, data, out, "exactly maxDim in both dimensions must not be treated as over the limit")
}

func TestShrinkToMax_DownscalesPreservingAspectRatio_PNG(t *testing.T) {
	data := encodePNG(t, 2048, 1024)

	out, err := imageresize.ShrinkToMax(data, "image/png", 1024)

	require.NoError(t, err)
	w, h := decodedDimensions(t, out)
	assert.Equal(t, 1024, w)
	assert.Equal(t, 512, h, "aspect ratio (2:1) must be preserved when scaling the wider dimension down to maxDim")
}

func TestShrinkToMax_DownscalesPreservingAspectRatio_TallImage(t *testing.T) {
	data := encodePNG(t, 1200, 2400)

	out, err := imageresize.ShrinkToMax(data, "image/png", 1024)

	require.NoError(t, err)
	w, h := decodedDimensions(t, out)
	assert.Equal(t, 512, w)
	assert.Equal(t, 1024, h, "the taller dimension must be the one clamped to maxDim")
}

func TestShrinkToMax_DownscalesJPEG(t *testing.T) {
	data := encodeJPEG(t, 1600, 1600)

	out, err := imageresize.ShrinkToMax(data, "image/jpeg", 1024)

	require.NoError(t, err)
	w, h := decodedDimensions(t, out)
	assert.Equal(t, 1024, w)
	assert.Equal(t, 1024, h)
	// Round-trip through image/jpeg to confirm it's still a valid, decodable JPEG.
	_, err = jpeg.Decode(bytes.NewReader(out))
	assert.NoError(t, err)
}

func TestShrinkToMax_RejectsHugeDeclaredDimensions(t *testing.T) {
	data := pngHeaderOnly(t, 20_000, 20_000)

	_, err := imageresize.ShrinkToMax(data, "image/png", 1024)

	require.Error(t, err, "a header declaring dimensions past the decode-bomb ceiling must be rejected before a full decode is attempted")
}

func TestShrinkToMax_RejectsUndecodableData(t *testing.T) {
	_, err := imageresize.ShrinkToMax([]byte("not an image"), "image/png", 1024)

	require.Error(t, err)
}
