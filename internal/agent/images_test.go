package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clauzette/internal/config"
	"clauzette/internal/llm"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newImageRig is a rogue rig with view_image registered, an image store,
// and a model with or without vision.
func newImageRig(t *testing.T, vision bool, replies ...func(ollama.ChatRequest) ollama.Message) *rogueRig {
	t.Helper()
	r := newRogueRig(t, replies...)
	ws, err := tools.NewWorkspace(r.root)
	if err != nil {
		t.Fatal(err)
	}
	r.a.Images = tools.NewImageStore(r.a.Store.ImageDir(), 0)
	r.a.Registry = tools.NewRegistry(&tools.WriteFile{WS: ws}, &tools.ViewImage{WS: ws, Store: r.a.Images})
	r.a.specs = r.a.Registry.Specs()
	r.a.rogueSpecs = append(append([]ollama.ToolSpec(nil), r.a.specs...), finishSpec(), restoreBackupSpec())
	useProfile(r, config.Profile{Name: "vl", Model: "vl:latest"}, Caps{Known: true, Tools: true, Vision: vision})
	return r
}

func (r *rogueRig) addImage(t *testing.T, name string, data []byte) llm.Image {
	t.Helper()
	p := filepath.Join(r.root, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := r.a.AddImageFile(p, name)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestImageInputSent(t *testing.T) {
	r := newImageRig(t, true, say("a red square"))
	data := pngBytes(t, 4, 3)
	img := r.addImage(t, "shot.png", data)
	in := Input{Text: "What is this?\n\n" + ImageNote([]llm.Image{img}), Images: []llm.Image{img}}
	if err := r.a.RunInput(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	req := r.model.request(0)
	last := req.Messages[len(req.Messages)-1]
	if len(last.Images) != 1 || last.Images[0] != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("image data not sent: %d images", len(last.Images))
	}
	if len(last.ImageRefs) != 0 {
		t.Fatal("image references were sent to Ollama")
	}
	if !hasTool(req, "view_image") {
		t.Fatal("view_image not offered to a model with vision")
	}

	// the session keeps the reference, never the data
	e := r.a.sess.Context[0]
	if len(e.ImageRefs) != 1 || e.ImageRefs[0].SHA256 != img.SHA256 || len(e.Images) != 0 {
		t.Fatalf("session entry %+v", e)
	}
	b, err := os.ReadFile(filepath.Join(r.a.Store.Dir, r.a.sess.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"images"`) || !strings.Contains(string(b), `"image_refs"`) {
		t.Fatal("the session file holds image data or no reference")
	}

	// /retry sends the image again
	got, err := r.a.UndoInput()
	if err != nil || got.Text != in.Text || len(got.Images) != 1 || got.Images[0].SHA256 != img.SHA256 {
		t.Fatalf("undo: %+v %v", got, err)
	}
}

func TestViewImageRoundTrip(t *testing.T) {
	var second ollama.ChatRequest
	r := newImageRig(t, true,
		func(ollama.ChatRequest) ollama.Message {
			return ollama.Message{Role: "assistant", Thinking: "I should look at it.",
				ToolCalls: []ollama.ToolCall{{Function: ollama.ToolCallFunction{Name: "view_image", Arguments: map[string]any{"path": "plot.png"}}}}}
		},
		func(req ollama.ChatRequest) ollama.Message {
			second = req
			return ollama.Message{Role: "assistant", Content: "the plot is empty"}
		},
	)
	data := pngBytes(t, 8, 8)
	os.WriteFile(filepath.Join(r.root, "plot.png"), data, 0o644)
	if err := r.a.RunTurn(context.Background(), "check plot.png"); err != nil {
		t.Fatal(err)
	}
	n := len(second.Messages)
	toolMsg, imgMsg := second.Messages[n-2], second.Messages[n-1]
	if toolMsg.Role != "tool" || !strings.Contains(toolMsg.Content, "Attached plot.png") {
		t.Fatalf("tool result %+v", toolMsg)
	}
	if imgMsg.Role != "user" || len(imgMsg.Images) != 1 || imgMsg.Images[0] != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("image message %+v", imgMsg)
	}
	if second.Messages[n-3].Thinking == "" {
		t.Fatal("the image message ended the turn: reasoning of the current turn was dropped")
	}
	if k := r.a.sess.Context[len(r.a.sess.Context)-2].Kind; k != session.KindImage {
		t.Fatalf("image message kind %q", k)
	}
	if text, err := r.a.Undo(); err != nil || text != "check plot.png" {
		t.Fatalf("undo took %q (%v), not the operator's message", text, err)
	}
}

func TestImagesWithoutVision(t *testing.T) {
	r := newImageRig(t, true, say("ok"), say("ok"))
	img := r.addImage(t, "shot.png", pngBytes(t, 4, 3))
	r.a.sess.Append(ollama.Message{Role: "user", Content: "look", ImageRefs: []llm.Image{img}}, "")
	r.a.sess.Append(ollama.Message{Role: "assistant", Content: "seen"}, "")

	useProfile(r, config.Profile{Name: "text", Model: "text:latest"}, Caps{Known: true, Tools: true})
	if err := r.a.RunTurn(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	req := r.model.request(0)
	if hasTool(req, "view_image") {
		t.Fatal("view_image offered to a model without vision")
	}
	m := req.Messages[1]
	if len(m.Images) != 0 || !strings.Contains(m.Content, "cannot see images") {
		t.Fatalf("image sent to a model without vision: %+v", m)
	}
	if !strings.Contains(r.a.ContextReport(), "cannot see them") {
		t.Fatal("/context does not say the images are not sent")
	}

	// back on a vision model, but the stored image is gone
	useProfile(r, config.Profile{Name: "vl", Model: "vl:latest"}, Caps{Known: true, Tools: true, Vision: true})
	os.Remove(filepath.Join(r.a.Images.Dir, img.SHA256+".png"))
	if err := r.a.RunTurn(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	m = r.model.request(1).Messages[1]
	if len(m.Images) != 0 || !strings.Contains(m.Content, "no longer available") {
		t.Fatalf("missing image: %+v", m)
	}
}

func TestImageTokensAndCompaction(t *testing.T) {
	r := newImageRig(t, true)
	img := r.addImage(t, "shot.png", pngBytes(t, 4, 3))
	for _, c := range []struct{ w, h, want int }{{400, 300, 138}, {1920, 1080, 2048}, {4, 3, 72}} {
		if got := r.a.imageTokens(llm.Image{Width: c.w, Height: c.h}); got != c.want {
			t.Errorf("%dx%d: %d tokens, want %d", c.w, c.h, got, c.want)
		}
	}
	plain := r.a.msgChars(ollama.Message{Role: "user", Content: "look"})
	with := r.a.msgChars(ollama.Message{Role: "user", Content: "look", ImageRefs: []llm.Image{img, img}})
	if got := r.a.tokens(with - plain); got < 2*r.a.imageTokens(img) {
		t.Fatalf("two images count ~%d tokens, want ~%d", got, 2*r.a.imageTokens(img))
	}
	r.a.Cfg.Tools.ImageTokens = 500
	if got := r.a.imageTokens(llm.Image{Width: 1920, Height: 1080}); got != 500 {
		t.Fatalf("tools.image_tokens not used: %d", got)
	}

	out := elide([]session.Entry{{Message: ollama.Message{Role: "user", Content: "look", ImageRefs: []llm.Image{img}}}}, 600)
	if len(out[0].ImageRefs) != 0 || !strings.Contains(out[0].Content, "1 image removed during context compaction: shot.png") {
		t.Fatalf("elided %+v", out[0].Message)
	}
	if !strings.Contains(renderEntry(session.Entry{Message: ollama.Message{Role: "user", Content: "[The image ...]"}, Kind: session.KindImage}), "IMAGES SHOWN") {
		t.Fatal("summary input does not mark the image message")
	}
}
