package tools

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// webpVP8X builds the header of an extended WebP with the given canvas.
func webpVP8X(w, h int) []byte {
	b := make([]byte, 30)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 22)
	copy(b[8:], "WEBP")
	copy(b[12:], "VP8X")
	binary.LittleEndian.PutUint32(b[16:], 10)
	put24 := func(at, v int) { b[at], b[at+1], b[at+2] = byte(v), byte(v>>8), byte(v>>16) }
	put24(24, w-1)
	put24(27, h-1)
	return b
}

func webpVP8(w, h int) []byte {
	b := make([]byte, 30)
	copy(b[0:], "RIFF")
	copy(b[8:], "WEBP")
	copy(b[12:], "VP8 ")
	b[23], b[24], b[25] = 0x9d, 0x01, 0x2a
	binary.LittleEndian.PutUint16(b[26:], uint16(w))
	binary.LittleEndian.PutUint16(b[28:], uint16(h))
	return b
}

func webpVP8L(w, h int) []byte {
	b := make([]byte, 30)
	copy(b[0:], "RIFF")
	copy(b[8:], "WEBP")
	copy(b[12:], "VP8L")
	b[20] = 0x2f
	v := uint32(w-1) | uint32(h-1)<<14
	binary.LittleEndian.PutUint32(b[21:], v)
	return b
}

func TestSniffImage(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 7, 5))
	var jb, gb bytes.Buffer
	jpeg.Encode(&jb, src, nil)
	gif.Encode(&gb, src, nil)
	cases := []struct {
		name string
		data []byte
		mime string
		w, h int
	}{
		{"png", testPNG(t, 4, 3), "image/png", 4, 3},
		{"jpeg", jb.Bytes(), "image/jpeg", 7, 5},
		{"gif", gb.Bytes(), "image/gif", 7, 5},
		{"webp vp8x", webpVP8X(1920, 1080), "image/webp", 1920, 1080},
		{"webp vp8", webpVP8(640, 480), "image/webp", 640, 480},
		{"webp vp8l", webpVP8L(300, 200), "image/webp", 300, 200},
	}
	for _, c := range cases {
		mime, w, h, err := SniffImage(c.data)
		if err != nil || mime != c.mime || w != c.w || h != c.h {
			t.Errorf("%s: %s %dx%d %v", c.name, mime, w, h, err)
		}
	}
	for name, data := range map[string][]byte{
		"text":          []byte("hello, not an image"),
		"svg":           []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		"truncated png": testPNG(t, 4, 3)[:12],
		"huge":          webpVP8X(10000, 10000),
	} {
		if _, _, _, err := SniffImage(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestImageStore(t *testing.T) {
	s := NewImageStore(filepath.Join(t.TempDir(), "images"), 0)
	data := testPNG(t, 4, 3)
	img, err := s.Add(data, "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if img.MIME != "image/png" || img.Width != 4 || img.Height != 3 || img.Bytes != int64(len(data)) || len(img.SHA256) != 64 {
		t.Fatalf("image %+v", img)
	}
	again, err := s.Add(data, "copy.png")
	if err != nil || again.SHA256 != img.SHA256 {
		t.Fatalf("same bytes stored again: %+v %v", again, err)
	}
	got, err := s.Data(img)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("data: %v", err)
	}
	st, _ := os.Stat(s.file(img))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("stored with mode %v", st.Mode().Perm())
	}

	// a reference cannot point outside the store, and changed bytes are refused
	bad := img
	bad.SHA256 = "../../etc/passwd"
	if _, err := s.Data(bad); err == nil {
		t.Fatal("invalid reference accepted")
	}
	os.WriteFile(s.file(img), []byte("tampered"), 0o600)
	if _, err := s.Data(img); err == nil {
		t.Fatal("tampered image accepted")
	}

	small := NewImageStore(t.TempDir(), 1024)
	if _, err := small.Add(testPNG(t, 300, 300), "big.png"); err == nil || !strings.Contains(err.Error(), "image limit") {
		t.Fatalf("over the limit: %v", err)
	}
}

func TestViewImage(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "shot.png"), testPNG(t, 4, 3), 0o644)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("text"), 0o644)
	outside := filepath.Join(t.TempDir(), "secret.png")
	os.WriteFile(outside, testPNG(t, 4, 3), 0o644)

	store := NewImageStore(filepath.Join(t.TempDir(), "images"), 0)
	tool := &ViewImage{WS: ws, Store: store}
	if tool.Risk() != ReadOnly {
		t.Fatal("view_image must be read-only")
	}
	ctx := context.Background()
	act, err := tool.Prepare(ctx, Args{"path": "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(ctx)
	if err != nil || !strings.Contains(out, "Attached shot.png (PNG, 4×3") {
		t.Fatalf("%q %v", out, err)
	}
	if p := store.Take(); len(p) != 1 || p[0].Path != "shot.png" {
		t.Fatalf("pending %+v", p)
	}
	if p := store.Take(); len(p) != 0 {
		t.Fatal("Take did not clear the pending images")
	}

	if _, err := tool.Prepare(ctx, Args{"path": outside}); err == nil {
		t.Fatal("an image outside the workspace was accepted")
	}
	if _, err := tool.Prepare(ctx, Args{"path": "../" + filepath.Base(filepath.Dir(outside)) + "/secret.png"}); err == nil {
		t.Fatal("a relative path out of the workspace was accepted")
	}
	act, _ = tool.Prepare(ctx, Args{"path": "notes.txt"})
	if _, err := act.Run(ctx); err == nil {
		t.Fatal("a text file was accepted as an image")
	}
	if p := store.Take(); len(p) != 0 {
		t.Fatal("a failed call queued an image")
	}
}
