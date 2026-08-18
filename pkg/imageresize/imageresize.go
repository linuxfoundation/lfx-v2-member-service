// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package imageresize downscales an already-validated raster logo image to a
// maximum width/height, preserving aspect ratio (LFXV2-2016, Eric Searcy's
// Monday sync spec: shrink rather than reject). SVG is vector, so it never
// routes through this package.
package imageresize

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"

	"golang.org/x/image/draw"
)

// maxDecodeDimensionPx and maxDecodePixels together bound what ShrinkToMax
// will decode. image.DecodeConfig reads only the header, so these checks run
// before the (memory-proportional-to-pixel-count) full image.Decode —
// otherwise a small compressed file could declare enormous pixel dimensions
// and exhaust memory on decode regardless of the on-disk size cap already
// enforced by MaxB2BOrgLogoSizeBytes. The per-axis check alone isn't enough:
// a 10,000x10,000 image passes it but is still 100 million pixels (~400MB
// decoded as RGBA), so maxDecodePixels bounds the total regardless of aspect
// ratio (LFXV2-2016 lfx-reviewer finding on PR #87).
const (
	maxDecodeDimensionPx = 10_000
	maxDecodePixels      = 40_000_000
)

// ShrinkToMax decodes data (already sniffed as mediaType — "image/png" or
// "image/jpeg") and, if either dimension exceeds maxDim, downscales it to fit
// within maxDim x maxDim while preserving aspect ratio, then re-encodes to
// the same format. If both dimensions are already within maxDim, data is
// returned unchanged.
func ShrinkToMax(data []byte, mediaType string, maxDim int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("imageresize: reading image header: %w", err)
	}
	if cfg.Width > maxDecodeDimensionPx || cfg.Height > maxDecodeDimensionPx {
		return nil, fmt.Errorf("imageresize: image dimensions %dx%d exceed the %dpx decode limit", cfg.Width, cfg.Height, maxDecodeDimensionPx)
	}
	if pixels := cfg.Width * cfg.Height; pixels > maxDecodePixels {
		return nil, fmt.Errorf("imageresize: image dimensions %dx%d (%d px) exceed the %d px decode limit", cfg.Width, cfg.Height, pixels, maxDecodePixels)
	}
	if cfg.Width <= maxDim && cfg.Height <= maxDim {
		return data, nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("imageresize: decoding image: %w", err)
	}

	scale := float64(maxDim) / math.Max(float64(cfg.Width), float64(cfg.Height))
	newW := int(math.Round(float64(cfg.Width) * scale))
	newH := int(math.Round(float64(cfg.Height) * scale))
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	switch mediaType {
	case "image/png":
		err = png.Encode(&buf, dst)
	case "image/jpeg":
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90})
	default:
		return nil, fmt.Errorf("imageresize: unsupported media type %q", mediaType)
	}
	if err != nil {
		return nil, fmt.Errorf("imageresize: encoding resized image: %w", err)
	}
	return buf.Bytes(), nil
}
