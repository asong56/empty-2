// media.go — Media Transcoding Worker Pool
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type MediaConfig struct {
	WorkerCount    int
	StorageDir     string
	FFmpegPath     string
	MaxQueueSize   int
	DefaultTTL     time.Duration
	CleanupInterval time.Duration
}

func DefaultMediaConfig() MediaConfig {
	return MediaConfig{
		WorkerCount:     3,
		StorageDir:      "/var/tarditalk/media",
		FFmpegPath:      "ffmpeg",
		MaxQueueSize:    64,
		DefaultTTL:      72 * time.Hour,
		CleanupInterval: 30 * time.Minute,
	}
}

type MediaJobType string

const (
	JobTypeImage MediaJobType = "image"
	JobTypeVideo MediaJobType = "video"
	JobTypeAudio MediaJobType = "audio"
)

type TranscodeJob struct {
	JobID        string
	Type         MediaJobType
	InputPath    string
	OutputDir    string
	OriginalSize int64
	ExpiresAt    time.Time
	PurgeOnView  bool
	EnqueuedAt   time.Time
}

type TranscodeResult struct {
	MediaRef MediaRef
	Err      error
}

type MediaWorkerPool struct {
	cfg     MediaConfig
	queue   chan TranscodeJob
	results sync.Map // jobID → TranscodeResult
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewMediaWorkerPool(cfg MediaConfig) *MediaWorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &MediaWorkerPool{
		cfg:    cfg,
		queue:  make(chan TranscodeJob, cfg.MaxQueueSize),
		ctx:    ctx,
		cancel: cancel,
	}
	if err := os.MkdirAll(cfg.StorageDir, 0700); err != nil {
		log.Printf("[media] storage dir: %v", err)
	}
	for i := 0; i < cfg.WorkerCount; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
	go p.janitor()
	return p
}

func (p *MediaWorkerPool) Shutdown() {
	p.cancel()
	p.wg.Wait()
}

func (p *MediaWorkerPool) Enqueue(
	jobType MediaJobType,
	inputPath string,
	originalSize int64,
	expiresAt time.Time,
	purgeOnView bool,
) (string, error) {
	jobID := newJobID()
	outputDir := filepath.Join(p.cfg.StorageDir, jobID)
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		return "", fmt.Errorf("create job dir: %w", err)
	}
	job := TranscodeJob{
		JobID: jobID, Type: jobType, InputPath: inputPath,
		OutputDir: outputDir, OriginalSize: originalSize,
		ExpiresAt: expiresAt, PurgeOnView: purgeOnView,
		EnqueuedAt: time.Now(),
	}
	select {
	case p.queue <- job:
		return jobID, nil
	default:
		return "", fmt.Errorf("media queue full")
	}
}

func (p *MediaWorkerPool) worker(id int) {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case job := <-p.queue:
			p.results.Store(job.JobID, p.process(job))
		}
	}
}

func (p *MediaWorkerPool) process(job TranscodeJob) TranscodeResult {
	switch job.Type {
	case JobTypeImage:
		return p.transcodeImage(job)
	case JobTypeVideo:
		return p.transcodeVideo(job)
	case JobTypeAudio:
		return p.transcodeAudio(job)
	default:
		return TranscodeResult{Err: fmt.Errorf("unknown job type: %s", job.Type)}
	}
}

func statFileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func writePurgeMarker(dir string, purge bool) {
	if purge {
		_ = os.WriteFile(filepath.Join(dir, ".purge"), []byte("1"), 0600)
	}
}

func buildResult(job TranscodeJob, path, mime, thumbURL string) TranscodeResult {
	writePurgeMarker(job.OutputDir, job.PurgeOnView)
	return TranscodeResult{MediaRef: MediaRef{
		ServerPath:     path,
		StreamURL:      fmt.Sprintf("/media/stream/%s", job.JobID),
		ThumbURL:       thumbURL,
		MIMEType:       mime,
		OriginalSize:   job.OriginalSize,
		TranscodedSize: statFileSize(path),
		ExpiresAt:      job.ExpiresAt,
		PurgeOnView:    job.PurgeOnView,
	}}
}

func (p *MediaWorkerPool) transcodeImage(job TranscodeJob) TranscodeResult {
	outputPath := filepath.Join(job.OutputDir, "output.webp")
	thumbPath := filepath.Join(job.OutputDir, "thumb.webp")

	if err := p.runFFmpeg(job.JobID, "-i", job.InputPath,
		"-vf", "scale=min(800\\,iw):-1", "-c:v", "libwebp", "-q:v", "60", "-y", outputPath,
	); err != nil {
		return TranscodeResult{Err: fmt.Errorf("image transcode: %w", err)}
	}
	_ = p.runFFmpeg(job.JobID, "-i", job.InputPath,
		"-vf", "scale=200:-1", "-c:v", "libwebp", "-q:v", "40", "-y", thumbPath,
	)
	return buildResult(job, outputPath, "image/webp", fmt.Sprintf("/media/thumb/%s", job.JobID))
}

func (p *MediaWorkerPool) transcodeVideo(job TranscodeJob) TranscodeResult {
	hlsDir := filepath.Join(job.OutputDir, "hls")
	if err := os.MkdirAll(hlsDir, 0700); err != nil {
		return TranscodeResult{Err: fmt.Errorf("create HLS dir: %w", err)}
	}
	m3u8Path := filepath.Join(hlsDir, "stream.m3u8")
	thumbPath := filepath.Join(job.OutputDir, "thumb.webp")

	if err := p.runFFmpeg(job.JobID,
		"-i", job.InputPath, "-c:v", "h264", "-c:a", "aac",
		"-profile:v", "baseline", "-level", "3.0",
		"-vf", "scale=-2:720", "-b:v", "1200k", "-b:a", "128k",
		"-start_number", "0", "-hls_time", "10", "-hls_list_size", "0",
		"-f", "hls", "-y", m3u8Path,
	); err != nil {
		return TranscodeResult{Err: fmt.Errorf("video transcode: %w", err)}
	}
	_ = p.runFFmpeg(job.JobID,
		"-ss", "2", "-i", job.InputPath, "-vframes", "1",
		"-vf", "scale=400:-1", "-c:v", "libwebp", "-q:v", "50", "-y", thumbPath,
	)
	return TranscodeResult{MediaRef: MediaRef{
		ServerPath: hlsDir,
		StreamURL:  fmt.Sprintf("/media/stream/%s/stream.m3u8", job.JobID),
		ThumbURL:   fmt.Sprintf("/media/thumb/%s", job.JobID),
		MIMEType:   "application/x-mpegURL",
		OriginalSize: job.OriginalSize, ExpiresAt: job.ExpiresAt,
	}}
}

func (p *MediaWorkerPool) transcodeAudio(job TranscodeJob) TranscodeResult {
	outputPath := filepath.Join(job.OutputDir, "output.ogg")
	if err := p.runFFmpeg(job.JobID,
		"-i", job.InputPath, "-c:a", "libopus", "-b:a", "32k", "-vbr", "on", "-y", outputPath,
	); err != nil {
		return TranscodeResult{Err: fmt.Errorf("audio transcode: %w", err)}
	}
	return buildResult(job, outputPath, "audio/ogg; codecs=opus", "")
}

func (p *MediaWorkerPool) runFFmpeg(jobID string, args ...string) error {
	fullArgs := append([]string{"-hide_banner", "-loglevel", "error"}, args...)
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.cfg.FFmpegPath, fullArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[media] ffmpeg job=%s error: %v\noutput: %s", jobID, err, output)
		return err
	}
	return nil
}

var purgeOnViewTracker sync.Map // jobID → *sync.Once

func (p *MediaWorkerPool) MediaHandler(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/media/"))
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	action, jobID := parts[0], parts[1]
	val, ok := p.results.Load(jobID)
	if !ok {
		http.Error(w, "job not found or pending", http.StatusNotFound)
		return
	}
	res := val.(TranscodeResult)
	if res.Err != nil {
		http.Error(w, fmt.Sprintf("transcode failed: %v", res.Err), http.StatusInternalServerError)
		return
	}
	ref := res.MediaRef
	switch action {
	case "stream":
		if len(parts) > 2 {
			http.ServeFile(w, r, filepath.Join(ref.ServerPath, strings.Join(parts[2:], "/")))
		} else {
			w.Header().Set("Content-Type", ref.MIMEType)
			p.maybePurge(jobID, ref.ServerPath)
			http.ServeFile(w, r, ref.ServerPath)
		}
	case "thumb":
		thumbPath := filepath.Join(p.cfg.StorageDir, jobID, "thumb.webp")
		if _, err := os.Stat(thumbPath); os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		http.ServeFile(w, r, thumbPath)
	default:
		http.NotFound(w, r)
	}
}

func (p *MediaWorkerPool) maybePurge(jobID, path string) {
	jobDir := filepath.Dir(path)
	markerPath := filepath.Join(jobDir, ".purge")
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		return
	}
	onceVal, _ := purgeOnViewTracker.LoadOrStore(jobID, &sync.Once{})
	onceVal.(*sync.Once).Do(func() {
		os.RemoveAll(jobDir)
		p.results.Delete(jobID)
	})
}

func (p *MediaWorkerPool) janitor() {
	ticker := time.NewTicker(p.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.sweepExpired()
		}
	}
}

func (p *MediaWorkerPool) sweepExpired() {
	now := time.Now()
	p.results.Range(func(k, v interface{}) bool {
		res, ok := v.(TranscodeResult)
		if !ok {
			return true
		}
		if now.After(res.MediaRef.ExpiresAt) && !res.MediaRef.ExpiresAt.IsZero() {
			jobID := k.(string)
			jobDir := filepath.Join(p.cfg.StorageDir, jobID)
			os.RemoveAll(jobDir)
			p.results.Delete(jobID)
			purgeOnViewTracker.Delete(jobID)
		}
		return true
	})
}

func newJobID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().UnixMicro()%1e6)
}

func extForMime(mime string) string {
	for prefix, ext := range map[string]string{
		"image/webp":".webp","image/jpeg":".jpg","image/png":".png",
		"video/mp4":".mp4","audio/ogg":".ogg","application/x-mpegURL":".m3u8",
	} {
		if strings.HasPrefix(mime, prefix) { return ext }
	}
	return ".bin"
}

func targetMimeForJobType(t MediaJobType) string {
	m := map[MediaJobType]string{JobTypeImage:"image/webp",JobTypeVideo:"application/x-mpegURL",JobTypeAudio:"audio/ogg; codecs=opus"}
	if v, ok := m[t]; ok { return v }
	return "application/octet-stream"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

func splitPath(path string) []string {
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
