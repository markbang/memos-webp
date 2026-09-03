// Command r2-webp-migrate batch-converts raster attachments already stored in
// an S3-compatible bucket (e.g. Cloudflare R2) into downscaled WebP, following
// the same rules the memos upload path applies to new attachments:
//
//   - JPEG/PNG/WebP objects are decoded (EXIF orientation baked in), downscaled
//     to a longest-edge cap (--max-dim), re-encoded as lossy WebP (--quality).
//   - Objects are overwritten in place under the SAME key, so existing
//     presigned URLs and database rows keep working. Overwriting removes the
//     original object (storage is reclaimed).
//   - Android motion-photo containers (embedded video), animated WebP and
//     undecodable blobs are skipped: re-encoding them would destroy content.
//
// Example:
//
//	r2-webp-migrate \
//	  --endpoint https://<account>.r2.cloudflarestorage.com \
//	  --bucket <bucket> \
//	  --prefix memos/ \
//	  --access-key AK... --secret-key SK... \
//	  --dry-run --verbose
//
// Credentials can also come from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
package main

import (
	"bytes"
	"context"
	"flag"
	"image"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	webp "github.com/SeriousBug/webp-go-pure/std"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/disintegration/imaging"

	"github.com/usememos/memos/internal/motionphoto"
)

type options struct {
	endpoint      string
	region        string
	bucket        string
	prefix        string
	accessKey     string
	secretKey     string
	maxDim        int
	quality       int
	effort        int
	dryRun        bool
	verbose       bool
	convertMotion bool
	keysFile      string
	concurrent    int
	limit         int
}

// Outcomes returned by processOne.
const (
	outcomeConverted   = "converted"
	outcomeNotRaster   = "skip:not-raster"
	outcomeMotion      = "skip:android-motion-photo"
	outcomeUndecodable = "skip:undecodable-header"
	outcomeAnimated    = "skip:animated-or-unsupported"
	outcomeSmallWebp   = "skip:already-small-webp"
	outcomeNoGain      = "skip:no-size-gain"
)

func main() {
	opts := &options{}
	flag.StringVar(&opts.endpoint, "endpoint", "", "S3-compatible endpoint URL (required)")
	flag.StringVar(&opts.region, "region", "auto", "S3 region (auto for R2)")
	flag.StringVar(&opts.bucket, "bucket", "", "bucket name (required)")
	flag.StringVar(&opts.prefix, "prefix", "", "object key prefix to scan")
	flag.StringVar(&opts.accessKey, "access-key", os.Getenv("AWS_ACCESS_KEY_ID"), "access key id")
	flag.StringVar(&opts.secretKey, "secret-key", os.Getenv("AWS_SECRET_ACCESS_KEY"), "secret access key")
	flag.IntVar(&opts.maxDim, "max-dim", 1920, "longest-edge pixel cap")
	flag.IntVar(&opts.quality, "quality", 85, "lossy WebP quality 1..100")
	flag.IntVar(&opts.effort, "effort", 4, "WebP encode effort 0..9 (higher = slower, smaller)")
	flag.BoolVar(&opts.dryRun, "dry-run", false, "list what would change without writing")
	flag.BoolVar(&opts.verbose, "verbose", false, "log every skip with reason and size")
	flag.BoolVar(&opts.convertMotion, "convert-motion", false, "also convert Android motion-photo containers (drops embedded video, keeps still)")
	flag.StringVar(&opts.keysFile, "keys-file", "", "process only the exact object keys listed in this file (one per line) instead of scanning the prefix")
	flag.IntVar(&opts.concurrent, "concurrency", 4, "parallel workers")
	flag.IntVar(&opts.limit, "limit", 0, "stop after this many converted objects (0 = unlimited)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if opts.endpoint == "" || opts.bucket == "" || opts.accessKey == "" || opts.secretKey == "" {
		slog.Error("endpoint, bucket and credentials are required")
		flag.Usage()
		os.Exit(2)
	}

	client, err := newS3Client(context.Background(), opts)
	if err != nil {
		slog.Error("failed to create S3 client", "error", err)
		os.Exit(1)
	}

	keys := make([]string, 0)
	if opts.keysFile != "" {
		raw, err := os.ReadFile(opts.keysFile)
		if err != nil {
			slog.Error("failed to read keys file", "error", err)
			os.Exit(1)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				keys = append(keys, line)
			}
		}
		slog.Info("keys file loaded", "keys", len(keys))
	} else {
		var err error
		keys, err = listObjects(context.Background(), client, opts)
		if err != nil {
			slog.Error("failed to list objects", "error", err)
			os.Exit(1)
		}
		slog.Info("scan complete", "candidates", len(keys), "prefix", opts.prefix)
	}

	var (
		wg          sync.WaitGroup
		sem         = make(chan struct{}, opts.concurrent)
		converted   atomic.Int64
		skipped     atomic.Int64
		failed      atomic.Int64
		bytesBefore atomic.Int64
		bytesAfter  atomic.Int64
	)
	for _, key := range keys {
		if opts.limit > 0 && converted.Load() >= int64(opts.limit) {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			defer func() { <-sem }()
			before, after, outcome, err := processOne(context.Background(), client, opts, key)
			if err != nil {
				failed.Add(1)
				slog.Error("failed", "key", key, "error", err)
				return
			}
			if outcome != outcomeConverted {
				skipped.Add(1)
				// Only surface raster skips (non-raster blobs are never
				// downloaded, so before==0 and we stay silent to avoid noise).
				if opts.verbose && before > 0 {
					slog.Info("skip", "outcome", outcome, "key", key, "size", before)
				}
				return
			}
			converted.Add(1)
			bytesBefore.Add(before)
			bytesAfter.Add(after)
			if opts.dryRun {
				slog.Info("[dry-run] would convert", "key", key, "before", before, "after", after)
			} else {
				slog.Info("converted", "key", key, "before", before, "after", after)
			}
		}(key)
	}
	wg.Wait()

	slog.Info("done",
		"converted", converted.Load(),
		"skipped", skipped.Load(),
		"failed", failed.Load(),
		"bytesBefore", bytesBefore.Load(),
		"bytesAfter", bytesAfter.Load(),
		"savedBytes", bytesBefore.Load()-bytesAfter.Load(),
		"dryRun", opts.dryRun,
	)
}

var rasterExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".webp": true,
}

var extMimeTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
}

func isRasterKey(key string) bool {
	return rasterExts[strings.ToLower(filepath.Ext(key))]
}

// processOne converts a single object. Returns before/after byte sizes (before
// is 0 when the object was never downloaded) and an outcome describing what
// happened.
func processOne(ctx context.Context, client *s3.Client, opts *options, key string) (before int64, after int64, outcome string, err error) {
	if !isRasterKey(key) {
		return 0, 0, outcomeNotRaster, nil
	}
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(opts.bucket), Key: aws.String(key)})
	if err != nil {
		return 0, 0, "", err
	}
	blob, err := io.ReadAll(out.Body)
	out.Body.Close()
	if err != nil {
		return 0, 0, "", err
	}
	before = int64(len(blob))
	ext := strings.ToLower(filepath.Ext(key))
	mimeType := extMimeTypes[ext]

	// Android motion photos embed a video; converting drops the video and
	// keeps the still frame. Only skipped unless --convert-motion is set.
	if mimeType == "image/jpeg" && motionphoto.DetectJPEG(blob) != nil && !opts.convertMotion {
		return before, 0, outcomeMotion, nil
	}

	// Cheap header check: skip anything undecodable and already-small WebP.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(blob))
	if err != nil {
		return before, 0, outcomeUndecodable, nil
	}
	if mimeType == "image/webp" && cfg.Width <= opts.maxDim && cfg.Height <= opts.maxDim {
		return before, 0, outcomeSmallWebp, nil
	}

	img, err := imaging.Decode(bytes.NewReader(blob), imaging.AutoOrientation(true))
	if err != nil {
		// Full decode refused (e.g. animated WebP) -> leave it alone.
		return before, 0, outcomeAnimated, nil
	}
	toEncode := img
	resized := false
	b := img.Bounds()
	if b.Dx() > opts.maxDim || b.Dy() > opts.maxDim {
		if b.Dx() >= b.Dy() {
			toEncode = imaging.Resize(img, opts.maxDim, 0, imaging.Lanczos)
		} else {
			toEncode = imaging.Resize(img, 0, opts.maxDim, imaging.Lanczos)
		}
		resized = true
	}

	var buf bytes.Buffer
	if err := webp.Encode(&buf, toEncode, &webp.Options{Quality: opts.quality, Effort: opts.effort}); err != nil {
		return before, 0, "", err
	}
	newBlob := buf.Bytes()
	if !resized && len(newBlob) >= len(blob) {
		return before, int64(len(newBlob)), outcomeNoGain, nil
	}

	if !opts.dryRun {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(opts.bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(newBlob),
			ContentType: aws.String("image/webp"),
		}); err != nil {
			return before, 0, "", err
		}
	}
	return before, int64(len(newBlob)), outcomeConverted, nil
}

func newS3Client(ctx context.Context, opts *options) (*s3.Client, error) {
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(opts.accessKey, opts.secretKey, "")),
		awsconfig.WithRegion(opts.region),
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(opts.endpoint)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}), nil
}

func listObjects(ctx context.Context, client *s3.Client, opts *options) ([]string, error) {
	var keys []string
	var continuation *string
	for {
		input := &s3.ListObjectsV2Input{
			Bucket: aws.String(opts.bucket),
			Prefix: aws.String(opts.prefix),
		}
		if continuation != nil {
			input.ContinuationToken = continuation
		}
		resp, err := client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, o := range resp.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		continuation = resp.NextContinuationToken
	}
	return keys, nil
}
