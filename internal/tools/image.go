package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the GIF header decoder for image.DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"clauzette/internal/llm"
	"clauzette/internal/textutil"
)

// DefaultImageMaxBytes is the largest image that can be attached.
const DefaultImageMaxBytes = 10 << 20

// maxImagePixels refuses images whose header claims more pixels than any
// screenshot or photo has: the model server would have to decode them.
const maxImagePixels = 50_000_000

// ImageStore keeps attached images under their SHA-256 (<dir>/<sha>.<ext>),
// so a session refers to the exact bytes the model saw: editing or deleting
// the original file does not change what a resumed session sends. Images
// the model asked to see (view_image) wait in a pending list until the
// agent attaches them to the conversation.
type ImageStore struct {
	Dir      string
	MaxBytes int64

	mu      sync.Mutex
	pending []llm.Image
}

func NewImageStore(dir string, maxBytes int64) *ImageStore {
	if maxBytes <= 0 {
		maxBytes = DefaultImageMaxBytes
	}
	return &ImageStore{Dir: dir, MaxBytes: maxBytes}
}

// SniffImage checks that b is a PNG, JPEG, WebP or GIF image (by its
// content, not its name) and reads its size from the header.
func SniffImage(b []byte) (mime string, width, height int, err error) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		mime = "image/png"
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		mime = "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		mime = "image/gif"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		mime = "image/webp"
		width, height, err = webpSize(b)
		if err != nil {
			return "", 0, 0, err
		}
	default:
		return "", 0, 0, errors.New("not a PNG, JPEG, WebP or GIF image")
	}
	if mime != "image/webp" {
		cfg, _, derr := image.DecodeConfig(bytes.NewReader(b))
		if derr != nil {
			return "", 0, 0, fmt.Errorf("damaged %s image: %v", strings.TrimPrefix(mime, "image/"), derr)
		}
		width, height = cfg.Width, cfg.Height
	}
	if width <= 0 || height <= 0 {
		return "", 0, 0, errors.New("the image has no size")
	}
	if width*height > maxImagePixels {
		return "", 0, 0, fmt.Errorf("the image is %d×%d, over the %d megapixel limit", width, height, maxImagePixels/1_000_000)
	}
	return mime, width, height, nil
}

// webpSize reads the canvas size of a WebP image (lossy VP8, lossless
// VP8L or extended VP8X), which the standard library cannot decode.
func webpSize(b []byte) (int, int, error) {
	bad := errors.New("damaged webp image")
	if len(b) < 30 {
		return 0, 0, bad
	}
	switch string(b[12:16]) {
	case "VP8 ":
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, bad
		}
		w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
		return w, h, nil
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, bad
		}
		w := 1 + (int(b[21]) | int(b[22]&0x3f)<<8)
		h := 1 + (int(b[22]>>6) | int(b[23])<<2 | int(b[24]&0x0f)<<10)
		return w, h, nil
	case "VP8X":
		w := 1 + (int(b[24]) | int(b[25])<<8 | int(b[26])<<16)
		h := 1 + (int(b[27]) | int(b[28])<<8 | int(b[29])<<16)
		return w, h, nil
	}
	return 0, 0, bad
}

func imageExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// DescribeImage renders an image's type, size and file size, e.g.
// "PNG, 1280×720, 245 KB".
func DescribeImage(img llm.Image) string {
	kind := strings.ToUpper(strings.TrimPrefix(img.MIME, "image/"))
	return fmt.Sprintf("%s, %d×%d, %s", kind, img.Width, img.Height, textutil.HumanBytes(img.Bytes))
}

func (s *ImageStore) tooBig(n int64, name string) error {
	if n > s.MaxBytes {
		return fmt.Errorf("%s is %s, over the %s image limit (tools.image_max_bytes); make a smaller copy first (for example with ImageMagick: convert in.png -resize 1600x1600 out.jpg)",
			name, textutil.HumanBytes(n), textutil.HumanBytes(s.MaxBytes))
	}
	return nil
}

// Add checks an image and stores it. from is where it came from, for display.
func (s *ImageStore) Add(data []byte, from string) (llm.Image, error) {
	if err := s.tooBig(int64(len(data)), from); err != nil {
		return llm.Image{}, err
	}
	mime, w, h, err := SniffImage(data)
	if err != nil {
		return llm.Image{}, fmt.Errorf("%s: %w", from, err)
	}
	sum := sha256.Sum256(data)
	img := llm.Image{Path: from, MIME: mime, SHA256: hex.EncodeToString(sum[:]), Width: w, Height: h, Bytes: int64(len(data))}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return llm.Image{}, fmt.Errorf("image store: %w", err)
	}
	dst := s.file(img)
	if _, err := os.Stat(dst); err == nil {
		return img, nil // the same bytes are stored already
	}
	tmp, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return llm.Image{}, fmt.Errorf("image store: %w", err)
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), dst)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return llm.Image{}, fmt.Errorf("image store: %w", werr)
	}
	return img, nil
}

// AddFile reads and stores an image file the operator named (/image). The
// operator may name any file they can read; the model's view_image tool is
// limited to the workspace.
func (s *ImageStore) AddFile(path, display string) (llm.Image, error) {
	st, err := os.Stat(path)
	if err != nil {
		return llm.Image{}, err
	}
	if st.IsDir() {
		return llm.Image{}, fmt.Errorf("%s is a directory", display)
	}
	if err := s.tooBig(st.Size(), display); err != nil {
		return llm.Image{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return llm.Image{}, err
	}
	return s.Add(b, display)
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (s *ImageStore) file(img llm.Image) string {
	return filepath.Join(s.Dir, img.SHA256+imageExt(img.MIME))
}

// Data returns a stored image's bytes, checked against its hash.
func (s *ImageStore) Data(img llm.Image) ([]byte, error) {
	if !validSHA(img.SHA256) || imageExt(img.MIME) == "" {
		return nil, errors.New("invalid image reference")
	}
	b, err := os.ReadFile(s.file(img))
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != img.SHA256 {
		return nil, errors.New("the stored image does not match its hash")
	}
	return b, nil
}

// Attach queues an image the model asked to see; the agent adds it to the
// conversation after the step's tool results.
func (s *ImageStore) Attach(img llm.Image) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, img)
}

// Take returns and clears the queued images.
func (s *ImageStore) Take() []llm.Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	s.pending = nil
	return p
}

// --------------------------------------------------------------- view_image

type ViewImage struct {
	WS    *Workspace
	Store *ImageStore
}

func (t *ViewImage) Name() string { return "view_image" }
func (t *ViewImage) Risk() Risk   { return ReadOnly }

func (t *ViewImage) Description() string {
	return "Look at an image file in the workspace (PNG, JPEG, WebP or GIF), such as a screenshot, " +
		"a chart you rendered or a photo. The image is shown to you in the next message. " +
		"For text files use read_file."
}

func (t *ViewImage) Parameters() map[string]any {
	return objectSchema([]string{"path"}, map[string]any{
		"path": prop("string", "Image path relative to the workspace root (absolute paths must be inside the workspace)."),
	})
}

func (t *ViewImage) Prepare(_ context.Context, args Args) (*Action, error) {
	p, err := args.Str("path", true)
	if err != nil {
		return nil, err
	}
	loc, err := t.WS.ResolveFile(p, AccessRead)
	if err != nil {
		return nil, err
	}
	rel := t.WS.Rel(loc.Abs)
	return &Action{
		Summary: "view image " + rel,
		Run: func(context.Context) (string, error) {
			st, err := loc.Stat()
			if err != nil {
				return "", cleanErr(err, rel)
			}
			if st.IsDir() {
				return "", fmt.Errorf("%s is a directory", rel)
			}
			if err := t.Store.tooBig(st.Size(), rel); err != nil {
				return "", err
			}
			b, err := loc.ReadFile()
			if err != nil {
				return "", cleanErr(err, rel)
			}
			img, err := t.Store.Add(b, rel)
			if err != nil {
				return "", err
			}
			t.Store.Attach(img)
			return fmt.Sprintf("Attached %s (%s); it is shown to you in the next message.", rel, DescribeImage(img)), nil
		},
	}, nil
}
