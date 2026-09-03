package v1

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	webp "github.com/SeriousBug/webp-go-pure/std"
	"github.com/disintegration/imaging"
	"github.com/pkg/errors"
)

const (
	// defaultImageMaxDimension is the longest-edge limit (in pixels) applied to
	// attachments before they are pushed to external storage. Phone originals
	// are commonly 4032px+ while the memos UI never renders them anywhere near
	// that size, so downscaling keeps visual quality while cutting bandwidth
	// and storage by an order of magnitude. Override with
	// MEMOS_IMAGE_MAX_DIMENSION.
	defaultImageMaxDimension = 1920

	// defaultWebpQuality is the lossy WebP quality target for optimized
	// attachments. Override with MEMOS_IMAGE_WEBP_QUALITY.
	defaultWebpQuality = 85

	// imageEncodeEffort trades WebP encode time for output size. Lossy effort
	// runs 0..9 (higher = slower + smaller). 4 is a good middle ground for a
	// personal instance that rarely uploads but values responsiveness.
	imageEncodeEffort = 4
)

// webpOptimizableTypes are raster formats we decode and re-encode as WebP.
// GIF is excluded because it may be animated; HEIC/TIFF are excluded because
// there is no pure-Go decoder wired up.
var webpOptimizableTypes = map[string]bool{
	"image/jpeg": true,
	"image/jpg":  true,
	"image/png":  true,
	"image/webp": true,
}

func isWebpOptimizableType(mimeType string) bool {
	return webpOptimizableTypes[mimeType]
}

func envPositiveInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func maxImageDimension() int {
	return envPositiveInt("MEMOS_IMAGE_MAX_DIMENSION", defaultImageMaxDimension)
}

func imageWebpQuality() int {
	q := envPositiveInt("MEMOS_IMAGE_WEBP_QUALITY", defaultWebpQuality)
	if q > 100 {
		q = 100
	}
	return q
}

type imageCompressResult struct {
	// Blob is the re-encoded WebP payload.
	Blob []byte
	// Resized reports whether the image was downscaled. When true the result
	// is always adopted (smaller dimensions are the point); when false the
	// result is adopted only if it is also smaller than the input.
	Resized bool
}

// compressAttachmentImage downscales (longest edge capped at
// MEMOS_IMAGE_MAX_DIMENSION) and transcodes a raster attachment to lossy WebP
// at MEMOS_IMAGE_WEBP_QUALITY. EXIF is dropped as a side effect of the
// re-encode, and EXIF orientation is baked in while decoding.
//
// A nil result (with nil error) means "keep the original bytes": the image is
// already a WebP within the dimension cap, or re-encoding would not save any
// bytes. It never returns an error for "not worth it" — only for failures such
// as undecodable or animated input, in which case the caller keeps the
// original blob untouched.
func compressAttachmentImage(imageData []byte, mimeType string) (*imageCompressResult, error) {
	if err := validateImagePixelCount(imageData); err != nil {
		return nil, err
	}

	// Decode with automatic EXIF orientation correction so the baked result is
	// upright. WebP decoding is registered by the webp-go-pure/std import.
	img, err := imaging.Decode(bytes.NewReader(imageData), imaging.AutoOrientation(true))
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode image")
	}
	if img == nil {
		return nil, errors.New("decoded image is nil")
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, errors.New("decoded image has empty bounds")
	}

	maxDim := maxImageDimension()
	srcIsWebp := mimeType == "image/webp"
	if width <= maxDim && height <= maxDim && srcIsWebp {
		// Nothing to gain: resizing is unnecessary and re-encoding would only
		// add generation loss.
		return nil, nil
	}

	toEncode := img
	resized := false
	if width > maxDim || height > maxDim {
		if width >= height {
			toEncode = imaging.Resize(img, maxDim, 0, imaging.Lanczos)
		} else {
			toEncode = imaging.Resize(img, 0, maxDim, imaging.Lanczos)
		}
		if toEncode == nil {
			return nil, errors.New("failed to resize image")
		}
		resized = true
	}

	var buf bytes.Buffer
	if err := webp.Encode(&buf, toEncode, &webp.Options{
		Quality: imageWebpQuality(),
		Effort:  imageEncodeEffort,
	}); err != nil {
		return nil, errors.Wrap(err, "failed to encode webp")
	}

	out := buf.Bytes()
	// Never adopt a larger output unless we actually downscaled (dimension
	// reduction still wins for bandwidth even if the file grew slightly).
	if !resized && len(out) >= len(imageData) {
		return nil, nil
	}
	return &imageCompressResult{Blob: out, Resized: resized}, nil
}

// replaceExtWithWebp swaps the extension of a filename to .webp while keeping
// the base name intact (e.g. "MVIMG_20260728_105103.jpg" ->
// "MVIMG_20260728_105103.webp").
func replaceExtWithWebp(filename string) string {
	ext := filepath.Ext(filename)
	if ext == "" {
		return filename + ".webp"
	}
	return strings.TrimSuffix(filename, ext) + ".webp"
}
