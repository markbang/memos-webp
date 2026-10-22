package motionphoto

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// ErrFFmpegUnavailable indicates that no ffmpeg binary was found on PATH.
// Motion clips can still be served, but they may stay in a codec the
// browser cannot decode.
var ErrFFmpegUnavailable = errors.New("ffmpeg not found in PATH")

// transcodeTimeout bounds a single re-encode. Motion clips are a few seconds
// long; anything approaching this limit is either pathological input or a
// stuck process.
const transcodeTimeout = 30 * time.Second

var (
	ffmpegOnce sync.Once
	ffmpegPath string
)

func resolveFFmpeg() (string, error) {
	ffmpegOnce.Do(func() {
		path, err := exec.LookPath("ffmpeg")
		if err == nil {
			ffmpegPath = path
		}
	})
	if ffmpegPath == "" {
		return "", ErrFFmpegUnavailable
	}
	return ffmpegPath, nil
}

// TranscodeToH264 re-encodes a motion clip as H.264 in an MP4 container.
//
// Android Motion Photos normally embed HEVC (hvc1), which Chromium and Firefox
// cannot decode, so the LIVE preview silently fails to start. H.264 with
// yuv420p plays everywhere, and dropped audio/metadata keeps the clip small.
// Resolution is preserved; only odd dimensions are rounded down for 4:2:0.
func TranscodeToH264(ctx context.Context, clip []byte) ([]byte, error) {
	ffmpeg, err := resolveFFmpeg()
	if err != nil {
		return nil, err
	}

	workDir, err := os.MkdirTemp("", "motion-transcode-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workDir)

	inputPath := filepath.Join(workDir, "input")
	outputPath := filepath.Join(workDir, "output.mp4")
	if err := os.WriteFile(inputPath, clip, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()

	cmd := exec.CommandContext(
		ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", inputPath,
		"-t", "10",
		"-map_metadata", "-1",
		"-an",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-pix_fmt", "yuv420p",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-movflags", "+faststart",
		outputPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New("ffmpeg failed: " + err.Error() + ": " + stderr.String())
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, err
	}
	if len(output) == 0 {
		return nil, errors.New("ffmpeg produced an empty clip")
	}
	return output, nil
}
