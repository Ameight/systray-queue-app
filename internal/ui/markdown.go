package ui

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/ncruces/zenity"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/Ameight/systray-queue-app/internal/queue"
)

// MakeTemplateIcon returns a minimal monochrome 16×16 PNG icon (suitable as macOS template icon).
func MakeTemplateIcon() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{0, 0, 0, 0}}, image.Point{}, draw.Src)
	fg := color.RGBA{0, 0, 0, 255}
	cx, cy, r := 8, 8, 4
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, fg)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// Error shows a native error dialog.
func Error(title, msg string) {
	_ = zenity.Error(msg, zenity.Title(title))
}

// QuickAddText shows a simple text-entry dialog for adding a task.
// Returns (text, true, nil) on OK, ("", false, nil) on cancel, ("", false, err) on error.
func QuickAddText() (string, bool, error) {
	text, err := zenity.Entry(
		"Task text:",
		zenity.Title("Add task"),
		zenity.OKLabel("Add"),
		zenity.CancelLabel("Cancel"),
	)
	if errors.Is(err, zenity.ErrCanceled) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false, nil
	}
	return text, true, nil
}

// RenderPage wraps body HTML in a full page with shared styles.
func RenderPage(title, body string) string {
	return `<!doctype html><html><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + title + `</title>
<style>
  body{font-family:-apple-system,Segoe UI,Roboto,Arial,sans-serif;line-height:1.6;padding:20px;max-width:860px;margin:0 auto;color:#1a1a1a}
  h1{font-size:20px;margin:0 0 16px}
  button{padding:8px 14px;border-radius:8px;border:1px solid #ccc;background:#fff;cursor:pointer;font-size:14px}
  button:hover{background:#f5f5f5}
  .row{display:flex;gap:8px;align-items:center;margin:12px 0;flex-wrap:wrap}
  .card{border:1px solid #e0e0e0;border-radius:12px;padding:16px 20px;margin:12px 0}
  .muted{color:#888;font-size:14px}
  textarea{width:100%;min-height:200px;font-family:inherit;font-size:14px;padding:10px;border:1px solid #ccc;border-radius:8px;box-sizing:border-box;resize:vertical}
  img{max-width:100%;height:auto;border-radius:8px;border:1px solid #ddd}
  pre,code{background:#f6f8fa;border-radius:4px;padding:2px 4px}
  pre code{padding:0}
  pre{padding:12px}
  audio{width:100%;margin:8px 0}
</style>
</head><body>` + body + `</body></html>`
}

// RenderTaskHTML renders a task's markdown content to an HTML fragment.
func RenderTaskHTML(t queue.Task) (string, error) {
	md := t.Text

	if t.AttachmentType == queue.AttachmentAudio && t.AttachmentPath != "" {
		audioURL := fileURLFromPath(t.AttachmentPath)
		md += "\n\n<audio controls src=\"" + audioURL + "\"></audio>\n"
	}

	gm := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)

	var out bytes.Buffer
	if err := gm.Convert([]byte(md), &out); err != nil {
		return "", err
	}

	return sanitizeHTML(out.String()), nil
}

func fileURLFromPath(p string) string {
	p = filepath.Clean(p)
	upath := filepath.ToSlash(p)
	if runtime.GOOS == "windows" {
		if len(upath) >= 2 && upath[1] == ':' {
			upath = "/" + upath
		}
	}
	u := url.URL{Scheme: "file", Path: upath}
	return u.String()
}

func sanitizeHTML(htmlStr string) string {
	p := bluemonday.UGCPolicy()

	p.AllowElements("audio", "source")
	p.AllowAttrs("controls").OnElements("audio")
	p.AllowAttrs("src").OnElements("audio", "source")
	p.AllowAttrs("type").OnElements("source")

	p.AllowElements("img")
	p.AllowAttrs("src", "alt", "title").OnElements("img")

	p.AllowURLSchemes("http", "https", "mailto", "file")

	return p.Sanitize(htmlStr)
}
