package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"clauzette/internal/llm"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

// Images
//
// An operator message can carry images (/image), and the model can ask to
// see an image in the workspace (view_image). Messages keep references
// (llm.Image); the bytes live in the image store (Deps.Images) and are
// added to the copy of each message that goes into a request. A model
// without vision gets a short note in place of each image, so a session
// can move between models.

// Input is an operator message: its text and any attached images.
type Input struct {
	Text   string
	Images []llm.Image
}

// imageTokens estimates what an image costs in the prompt. Qwen-style
// vision encoders use about one token per 32×32-pixel block (measured on
// Qwen3.8: 400×300 ≈ 120 tokens, 1920×1080 ≈ 2040); a server that scales
// big images down uses fewer, so the estimate errs high. tools.image_tokens
// replaces it with a fixed number for other encoders.
func (a *Agent) imageTokens(img llm.Image) int {
	if n := a.Cfg.Tools.ImageTokens; n > 0 {
		return n
	}
	blocks := func(px int) int { return (px + 31) / 32 }
	n := blocks(img.Width) * blocks(img.Height)
	if n < 64 {
		n = 64
	}
	return n + 8 // the markers around the image
}

// imagesChars is the images' size in the character-based estimate.
func (a *Agent) imagesChars(imgs []llm.Image) int {
	n := 0
	for _, img := range imgs {
		n += a.imageTokens(img)
	}
	return int(float64(n) * a.cpt)
}

// Vision reports whether the active model can see images.
func (a *Agent) Vision() bool { return a.Models.Active().Caps.Vision }

// AddImageFile stores an image file the operator attached with /image.
func (a *Agent) AddImageFile(path, display string) (llm.Image, error) {
	if a.Images == nil {
		return llm.Image{}, fmt.Errorf("images are not available (no image store)")
	}
	img, err := a.Images.AddFile(path, display)
	if err != nil {
		return llm.Image{}, err
	}
	a.Audit.Log(map[string]any{"session": a.sess.ID, "event": "image", "source": "operator", "path": display, "sha256": img.SHA256, "bytes": img.Bytes})
	return img, nil
}

func imageList(imgs []llm.Image) string {
	parts := make([]string, len(imgs))
	for i, img := range imgs {
		parts[i] = fmt.Sprintf("%s (%s)", img.Path, tools.DescribeImage(img))
	}
	return strings.Join(parts, ", ")
}

// imagesRemoved is the placeholder compaction leaves for removed images.
func imagesRemoved(imgs []llm.Image) string {
	return fmt.Sprintf("[%s removed during context compaction: %s; use view_image to look again]", countOf(len(imgs), "image"), imageList(imgs))
}

func countOf(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ImageNote is the text of an operator message for an attached image.
func ImageNote(imgs []llm.Image) string {
	return fmt.Sprintf("[Attached %s: %s]", map[bool]string{true: "image", false: "images"}[len(imgs) == 1], imageList(imgs))
}

// attachImages fills in the image data on the request copies of the
// messages. Images the model cannot see, or that are no longer stored,
// become a note in the text.
func (a *Agent) attachImages(msgs []ollama.Message) []ollama.Message {
	vision := a.Vision()
	for i := range msgs {
		m := &msgs[i]
		if len(m.ImageRefs) == 0 {
			continue
		}
		refs := m.ImageRefs
		m.ImageRefs, m.Images = nil, nil
		var notes []string
		for _, img := range refs {
			if !vision {
				notes = append(notes, fmt.Sprintf("[Image %s not shown: the current model (%s) cannot see images.]", img.Path, a.Models.Active().Model))
				continue
			}
			var b []byte
			err := fmt.Errorf("no image store")
			if a.Images != nil {
				b, err = a.Images.Data(img)
			}
			if err != nil {
				notes = append(notes, fmt.Sprintf("[Image %s is no longer available.]", img.Path))
				continue
			}
			m.ImageRefs = append(m.ImageRefs, img)
			m.Images = append(m.Images, base64.StdEncoding.EncodeToString(b))
		}
		if len(notes) > 0 {
			m.Content = strings.TrimSpace(m.Content + "\n\n" + strings.Join(notes, "\n"))
		}
	}
	return msgs
}

// RunInput is RunTurn for a message with images.
func (a *Agent) RunInput(ctx context.Context, in Input) error {
	return a.runTurnWith(ctx, in.Text, "", in.Images)
}

// takeToolImages adds the images the model asked to see in this step as
// one message after the tool results. OpenAI-style APIs accept images only
// in user messages, so they cannot go into the tool results themselves.
func (a *Agent) takeToolImages() {
	if a.Images == nil {
		return
	}
	imgs := a.Images.Take()
	if len(imgs) == 0 {
		return
	}
	for _, img := range imgs {
		a.Audit.Log(map[string]any{"session": a.sess.ID, "event": "image", "source": "view_image", "path": img.Path, "sha256": img.SHA256, "bytes": img.Bytes})
	}
	text := fmt.Sprintf("[%s you asked to see with view_image: %s]", map[bool]string{true: "The image", false: "The images"}[len(imgs) == 1], imageList(imgs))
	a.sess.Append(ollama.Message{Role: "user", Content: text, ImageRefs: imgs}, session.KindImage)
}

// withoutVision drops view_image for a model without vision.
func withoutVision(specs []ollama.ToolSpec) []ollama.ToolSpec {
	out := make([]ollama.ToolSpec, 0, len(specs))
	for _, s := range specs {
		if s.Function.Name != "view_image" {
			out = append(out, s)
		}
	}
	return out
}
