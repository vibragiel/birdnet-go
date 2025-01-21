package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/tphakala/birdnet-go/internal/conf"
)

// MaxClipNameLength is the maximum allowed length for a clip name
const MaxClipNameLength = 255

// AllowedCharacters is a regex pattern for allowed characters in clip names
const AllowedCharacters = `^[a-zA-Z0-9_/.-]+$`

var (
	ErrEmptyClipName     = errors.New("empty clip name")
	ErrClipNameTooLong   = errors.New("clip name too long")
	ErrInvalidCharacters = errors.New("invalid characters in clip name")
	ErrPathTraversal     = errors.New("path traversal attempt detected")
)

// sanitizeClipName performs sanity checks on the clip name and ensures it's a relative path
func (h *Handlers) sanitizeClipName(clipName string) (string, error) {
	// Check if the clip name is empty
	if clipName == "" {
		return "", ErrEmptyClipName
	}

	// Decode the clip name
	decodedClipName, err := url.QueryUnescape(clipName)
	if err != nil {
		return "", fmt.Errorf("error decoding clip name: %w", err)
	}
	h.Debug("sanitizeClipName: Decoded clip name: %s", decodedClipName)

	// Check the length of the decoded clip name
	if len(decodedClipName) > MaxClipNameLength {
		return "", ErrClipNameTooLong
	}

	// Check for allowed characters
	if !regexp.MustCompile(AllowedCharacters).MatchString(decodedClipName) {
		h.Debug("sanitizeClipName: Invalid characters in clip name: %s", decodedClipName)
		return "", ErrInvalidCharacters
	}

	// Clean the path and ensure it's relative
	cleanPath := filepath.Clean(decodedClipName)
	h.Debug("sanitizeClipName: Cleaned path: %s", cleanPath)

	if strings.Contains(cleanPath, "..") {
		h.Debug("sanitizeClipName: Path traversal attempt detected: %s", cleanPath)
		return "", ErrPathTraversal
	}

	// Remove 'clips/' prefix if present
	cleanPath = strings.TrimPrefix(cleanPath, "clips/")
	h.Debug("sanitizeClipName: Path after removing clips prefix: %s", cleanPath)

	// If the path is absolute, make it relative to the export path
	if filepath.IsAbs(cleanPath) {
		h.Debug("sanitizeClipName: Found absolute path: %s", cleanPath)
		exportPath := conf.Setting().Realtime.Audio.Export.Path
		h.Debug("sanitizeClipName: Export path from settings: %s", exportPath)

		if strings.HasPrefix(cleanPath, exportPath) {
			// Remove the export path prefix to make it relative
			cleanPath = strings.TrimPrefix(cleanPath, exportPath)
			cleanPath = strings.TrimPrefix(cleanPath, string(os.PathSeparator))
			h.Debug("sanitizeClipName: Converted to relative path: %s", cleanPath)
		} else {
			h.Debug("sanitizeClipName: Absolute path not under export directory: %s", cleanPath)
			return "", fmt.Errorf("invalid path: absolute path not under export directory")
		}
	}

	// Convert to forward slashes for web URLs
	cleanPath = filepath.ToSlash(cleanPath)
	h.Debug("sanitizeClipName: Final path with forward slashes: %s", cleanPath)

	return cleanPath, nil
}

// getFullPath returns the full filesystem path for a relative clip path
func getFullPath(relativePath string) string {
	exportPath := conf.Setting().Realtime.Audio.Export.Path
	return filepath.Join(exportPath, relativePath)
}

// getWebPath converts a filesystem path to a web-safe path
func getWebPath(path string) string {
	// Convert absolute path to relative path if it starts with the export path
	exportPath := conf.Setting().Realtime.Audio.Export.Path
	if strings.HasPrefix(path, exportPath) {
		path = strings.TrimPrefix(path, exportPath)
		path = strings.TrimPrefix(path, string(os.PathSeparator))
	}

	// Convert path separators to forward slashes for web URLs
	return filepath.ToSlash(path)
}

// Thumbnail returns the URL of a given bird's thumbnail image.
// It takes the bird's scientific name as input and returns the image URL as a string.
// If the image is not found or an error occurs, it returns an empty string.
func (h *Handlers) Thumbnail(scientificName string) string {
	if h.BirdImageCache == nil {
		// Return empty string if the cache is not initialized
		return ""
	}

	birdImage, err := h.BirdImageCache.Get(scientificName)
	if err != nil {
		// Return empty string if an error occurs
		return ""
	}

	return birdImage.URL
}

// ThumbnailAttribution returns the HTML-formatted attribution for a bird's thumbnail image.
// It takes the bird's scientific name as input and returns a template.HTML string.
// If the attribution information is incomplete or an error occurs, it returns an empty template.HTML.
func (h *Handlers) ThumbnailAttribution(scientificName string) template.HTML {
	if h.BirdImageCache == nil {
		// Return empty string if the cache is not initialized
		return template.HTML("")
	}

	birdImage, err := h.BirdImageCache.Get(scientificName)
	if err != nil {
		log.Printf("Error getting thumbnail info for %s: %v", scientificName, err)
		return template.HTML("")
	}

	if birdImage.AuthorName == "" || birdImage.LicenseName == "" {
		return template.HTML("")
	}

	var attribution string
	if birdImage.AuthorURL == "" {
		attribution = fmt.Sprintf("© %s / <a href=\"%q\">%s</a>",
			html.EscapeString(birdImage.AuthorName),
			html.EscapeString(birdImage.LicenseURL),
			html.EscapeString(birdImage.LicenseName))
	} else {
		attribution = fmt.Sprintf("© <a href=\"%q\">%s</a> / <a href=\"%q\">%s</a>",
			html.EscapeString(birdImage.AuthorURL),
			html.EscapeString(birdImage.AuthorName),
			html.EscapeString(birdImage.LicenseURL),
			html.EscapeString(birdImage.LicenseName))
	}

	return template.HTML(attribution)
}

// ServeSpectrogram serves or generates a spectrogram for a given clip.
func (h *Handlers) ServeSpectrogram(c echo.Context) error {
	// Extract clip name from the query parameters
	clipName := c.QueryParam("clip")

	// Sanitize the clip name
	sanitizedClipName, err := h.sanitizeClipName(clipName)
	if err != nil {
		h.Debug("Error sanitizing clip name: %v", err)
		return c.File("assets/images/spectrogram-placeholder.svg")
	}

	// Get the full path to the audio file
	fullPath := getFullPath(sanitizedClipName)

	// Construct the path to the spectrogram image
	spectrogramPath, err := h.getSpectrogramPath(fullPath, 400) // Assuming 400px width
	if err != nil {
		h.Debug("Error getting spectrogram path: %v", err)
		return c.File("assets/images/spectrogram-placeholder.svg")
	}

	// Serve the spectrogram image file
	return c.File(spectrogramPath)
}

// getSpectrogramPath generates the path to the spectrogram image file for a given audio file
func (h *Handlers) getSpectrogramPath(audioFileName string, width int) (string, error) {
	// Generate file paths
	dir := filepath.Dir(audioFileName)
	baseNameWithoutExt := strings.TrimSuffix(filepath.Base(audioFileName), filepath.Ext(audioFileName))
	spectrogramFileName := fmt.Sprintf("%s_%dpx.png", baseNameWithoutExt, width)
	spectrogramPath := filepath.Join(dir, spectrogramFileName)

	// Convert to web-friendly path
	webFriendlyPath := strings.ReplaceAll(spectrogramPath, string(os.PathSeparator), "/")

	// Check if the spectrogram already exists
	if spectrogramExists, err := fileExists(spectrogramPath); err != nil {
		return "", fmt.Errorf("error checking spectrogram file: %w", err)
	} else if spectrogramExists {
		return webFriendlyPath, nil
	}

	// Check if the original audio file exists
	if audioExists, err := fileExists(audioFileName); err != nil {
		log.Printf("error checking audio file: %s", err)
		return "", fmt.Errorf("error checking audio file: %w", err)
	} else if !audioExists {
		log.Printf("audio file does not exist: %s", audioFileName)
		return "", fmt.Errorf("audio file does not exist: %s", audioFileName)
	}

	// Create the spectrogram
	if err := createSpectrogramWithSoX(audioFileName, spectrogramPath, width); err != nil {
		log.Printf("error creating spectrogram: %s", err)
		return "", fmt.Errorf("error creating spectrogram: %w", err)
	}

	return webFriendlyPath, nil
}

// fileExists checks if a file exists and is not a directory
func fileExists(filename string) (bool, error) {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

// createSpectrogramWithSoX generates a spectrogram for an audio file using ffmpeg and SoX.
// It supports various audio formats by using ffmpeg to pipe the audio to SoX when necessary.
func createSpectrogramWithSoX(audioClipPath, spectrogramPath string, width int) error {
	// Get ffmpeg and sox paths from settings
	ffmpegBinary := conf.Setting().Realtime.Audio.FfmpegPath
	soxBinary := conf.Setting().Realtime.Audio.SoxPath

	// Verify ffmpeg and SoX paths
	if ffmpegBinary == "" {
		return fmt.Errorf("ffmpeg path not set in settings")
	}
	if soxBinary == "" {
		return fmt.Errorf("SoX path not set in settings")
	}

	// Set height based on width
	heightStr := strconv.Itoa(width / 2)
	widthStr := strconv.Itoa(width)

	// Determine if we need to use ffmpeg based on file extension
	ext := strings.ToLower(filepath.Ext(audioClipPath))
	// remove prefix dot
	ext = strings.TrimPrefix(ext, ".")
	useFFmpeg := true
	for _, soxType := range conf.Setting().Realtime.Audio.SoxAudioTypes {
		if ext == strings.ToLower(soxType) {
			useFFmpeg = false
			break
		}
	}

	var cmd *exec.Cmd
	var soxCmd *exec.Cmd

	// Decode audio using ffmpeg and pipe to sox for spectrogram creation
	if useFFmpeg {
		// Build ffmpeg command arguments
		ffmpegArgs := []string{"-hide_banner", "-i", audioClipPath, "-f", "sox", "-"}

		// Build SoX command arguments
		soxArgs := append([]string{"-t", "sox", "-"}, getSoxSpectrogramArgs(widthStr, heightStr, spectrogramPath)...)

		// Set up commands
		if runtime.GOOS == "windows" {
			cmd = exec.Command(ffmpegBinary, ffmpegArgs...)
			soxCmd = exec.Command(soxBinary, soxArgs...)
		} else {
			cmd = exec.Command("nice", append([]string{"-n", "19", ffmpegBinary}, ffmpegArgs...)...)
			soxCmd = exec.Command("nice", append([]string{"-n", "19", soxBinary}, soxArgs...)...)
		}

		// Set up pipe between ffmpeg and sox
		var err error
		soxCmd.Stdin, err = cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("error creating pipe: %w", err)
		}

		// Capture combined output
		var ffmpegOutput, soxOutput bytes.Buffer
		cmd.Stderr = &ffmpegOutput
		soxCmd.Stderr = &soxOutput

		// Allow other goroutines to run before starting SoX
		runtime.Gosched()

		// Start sox command
		if err := soxCmd.Start(); err != nil {
			log.Printf("SoX cmd: %s", soxCmd.String())
			return fmt.Errorf("error starting SoX command: %w", err)
		}

		// Define error message template
		const errFFmpegSoxFailed = "ffmpeg command failed: %v\nffmpeg output: %s\nsox output: %s\n%s"

		// Run ffmpeg command
		if err := cmd.Run(); err != nil {
			// Stop the SoX command to clean up resources
			if killErr := soxCmd.Process.Kill(); killErr != nil {
				log.Printf("Failed to kill SoX process: %v", killErr)
			}

			// Wait for SoX to finish and collect its error, if any
			waitErr := soxCmd.Wait()

			// Prepare additional error information
			var additionalInfo string
			if waitErr != nil && !errors.Is(waitErr, os.ErrProcessDone) {
				additionalInfo = fmt.Sprintf("sox wait error: %v", waitErr)
			}

			// Use fmt.Errorf with the constant format string
			return fmt.Errorf(errFFmpegSoxFailed, err, ffmpegOutput.String(), soxOutput.String(), additionalInfo)
		}

		// Allow other goroutines to run before waiting for SoX to finish
		runtime.Gosched()

		// Wait for sox command to finish
		if err := soxCmd.Wait(); err != nil {
			return fmt.Errorf("SoX command failed: %w\nffmpeg output: %s\nsox output: %s", err, ffmpegOutput.String(), soxOutput.String())
		}

		// Allow other goroutines to run after SoX finishes
		runtime.Gosched()
	} else {
		// Use SoX directly for supported formats
		soxArgs := append([]string{audioClipPath}, getSoxSpectrogramArgs(widthStr, heightStr, spectrogramPath)...)

		if runtime.GOOS == "windows" {
			soxCmd = exec.Command(soxBinary, soxArgs...)
		} else {
			soxCmd = exec.Command("nice", append([]string{"-n", "19", soxBinary}, soxArgs...)...)
		}

		// Capture output
		var soxOutput bytes.Buffer
		soxCmd.Stderr = &soxOutput
		soxCmd.Stdout = &soxOutput

		// Allow other goroutines to run before running SoX
		runtime.Gosched()

		// Run SoX command
		if err := soxCmd.Run(); err != nil {
			return fmt.Errorf("SoX command failed: %w\nOutput: %s", err, soxOutput.String())
		}

		// Allow other goroutines to run after SoX finishes
		runtime.Gosched()
	}

	return nil
}

// getSoxSpectrogramArgs returns the common SoX arguments for generating a spectrogram
func getSoxSpectrogramArgs(widthStr, heightStr, spectrogramPath string) []string {
	// TODO: make these dynamic based on audio length and gain
	const audioLength = "15"
	const dynamicRange = "100"

	args := []string{"-n", "rate", "24k", "spectrogram", "-x", widthStr, "-y", heightStr, "-d", audioLength, "-z", dynamicRange, "-o", spectrogramPath}
	width, _ := strconv.Atoi(widthStr)
	if width < 800 {
		args = append(args, "-r")
	}
	return args
}

// createSpectrogramWithFFmpeg generates a spectrogram for an audio file using only ffmpeg.
// It supports various audio formats and applies the same practices as createSpectrogramWithSoX.
func createSpectrogramWithFFmpeg(audioClipPath, spectrogramPath string, width int) error {
	// Get ffmpeg path from settings
	ffmpegBinary := conf.Setting().Realtime.Audio.FfmpegPath

	// Verify ffmpeg path
	if ffmpegBinary == "" {
		return fmt.Errorf("ffmpeg path not set in settings")
	}

	// Set height based on width
	height := width / 2
	heightStr := strconv.Itoa(height)
	widthStr := strconv.Itoa(width)

	// Build ffmpeg command arguments
	ffmpegArgs := []string{
		"-hide_banner",
		"-y", // answer yes to overwriting the output file if it already exists
		"-i", audioClipPath,
		"-lavfi", fmt.Sprintf("showspectrumpic=s=%sx%s:legend=0:gain=3:drange=100", widthStr, heightStr),
		"-frames:v", "1", // Generate only one frame instead of animation
		spectrogramPath,
	}

	// Determine the command based on the OS
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Directly use ffmpeg command on Windows
		cmd = exec.Command(ffmpegBinary, ffmpegArgs...)
	} else {
		// Prepend 'nice' to the command on Unix-like systems
		cmd = exec.Command("nice", append([]string{"-n", "19", ffmpegBinary}, ffmpegArgs...)...)
	}

	log.Printf("ffmpeg command: %s", cmd.String())

	// Capture combined output
	var output bytes.Buffer
	cmd.Stderr = &output
	cmd.Stdout = &output

	// Run ffmpeg command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg command failed: %w\nOutput: %s", err, output.String())
	}

	return nil
}

// sanitizeContentDispositionFilename sanitizes a filename for use in Content-Disposition header
func sanitizeContentDispositionFilename(filename string) string {
	// Remove any characters that could cause issues in headers
	// Replace quotes with single quotes, remove control characters, and escape special characters
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r == '"':
			return '\''
		case r < 32: // Control characters
			return -1
		case r == '\\' || r == '/' || r == ':' || r == '*' || r == '?' || r == '<' || r == '>' || r == '|':
			return '_'
		default:
			return r
		}
	}, filename)

	// URL encode the filename to handle non-ASCII characters
	encoded := url.QueryEscape(sanitized)

	return encoded
}

// ServeAudioClip serves an audio clip file
func (h *Handlers) ServeAudioClip(c echo.Context) error {
	h.Debug("ServeAudioClip: Starting to handle request for path: %s", c.Request().URL.String())

	// Extract clip name from the query parameters
	clipName := c.QueryParam("clip")
	h.Debug("ServeAudioClip: Raw clip name from query: %s", clipName)

	// Sanitize the clip name
	sanitizedClipName, err := h.sanitizeClipName(clipName)
	if err != nil {
		h.Debug("ServeAudioClip: Error sanitizing clip name: %v", err)
		c.Response().Header().Set(echo.HeaderContentType, "text/plain")
		return c.String(http.StatusNotFound, "Audio file not found")
	}
	h.Debug("ServeAudioClip: Sanitized clip name: %s", sanitizedClipName)

	// Get the full path to the audio file
	fullPath := getFullPath(sanitizedClipName)
	h.Debug("ServeAudioClip: Full path: %s", fullPath)

	// Verify that the full path is within the export directory
	absFullPath, err := filepath.Abs(fullPath)
	if err != nil {
		h.Debug("ServeAudioClip: Error obtaining absolute path: %v", err)
		return c.String(http.StatusInternalServerError, "Internal server error")
	}
	absExportPath, err := filepath.Abs(conf.Setting().Realtime.Audio.Export.Path)
	if err != nil {
		h.Debug("ServeAudioClip: Error obtaining absolute export path: %v", err)
		return c.String(http.StatusInternalServerError, "Internal server error")
	}
	if !strings.HasPrefix(absFullPath, absExportPath) {
		h.Debug("ServeAudioClip: Resolved path outside export directory: %s", absFullPath)
		return c.String(http.StatusForbidden, "Forbidden")
	}

	// Check if the file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			h.Debug("ServeAudioClip: Audio file not found: %s", fullPath)
		} else {
			h.Debug("ServeAudioClip: Error checking audio file: %v", err)
		}
		c.Response().Header().Set(echo.HeaderContentType, "text/plain")
		return c.String(http.StatusNotFound, "Audio file not found")
	}
	h.Debug("ServeAudioClip: File exists at path: %s", fullPath)

	// Get the filename for Content-Disposition
	filename := filepath.Base(sanitizedClipName)
	safeFilename := sanitizeContentDispositionFilename(filename)
	h.Debug("ServeAudioClip: Using filename for disposition: %s (safe: %s)", filename, safeFilename)

	// Get MIME type
	mimeType := getAudioMimeType(fullPath)
	h.Debug("ServeAudioClip: MIME type for file: %s", mimeType)

	// Set response headers
	c.Response().Header().Set(echo.HeaderContentType, mimeType)
	c.Response().Header().Set("Content-Transfer-Encoding", "binary")
	c.Response().Header().Set("Content-Description", "File Transfer")
	// Set both ASCII and UTF-8 versions of the filename for better browser compatibility
	c.Response().Header().Set(echo.HeaderContentDisposition,
		fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
			safeFilename,
			safeFilename))

	h.Debug("ServeAudioClip: Set headers - Content-Type: %s, Content-Disposition: %s",
		c.Response().Header().Get(echo.HeaderContentType),
		c.Response().Header().Get(echo.HeaderContentDisposition))

	// Serve the file
	h.Debug("ServeAudioClip: Attempting to serve file: %s", fullPath)
	return c.File(fullPath)
}

// getAudioMimeType returns the MIME type for an audio file based on its extension
func getAudioMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp3":
		return "audio/mpeg"
	case ".ogg", ".opus":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".flac":
		return "audio/flac"
	case ".aac":
		return "audio/aac"
	case ".m4a":
		return "audio/mp4"
	case ".alac":
		return "audio/x-alac"
	default:
		return "application/octet-stream"
	}
}
