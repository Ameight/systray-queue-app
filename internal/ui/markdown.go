package ui

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

func renderMarkdownToTempHTML(md string, maybeAudioPath string) (string, error) {
	// Добавим аудио-плеер, если есть вложение-аудио И оно ещё не упомянуто в тексте
	if maybeAudioPath != "" && !strings.Contains(md, maybeAudioPath) {
		md += "\n\n<audio controls src=\"" + pathToFileURL(maybeAudioPath) + "\"></audio>\n"
	}

	gm := goldmark.New(
		goldmark.WithExtensions(extension.GFM),          // таблицы/чекбоксы/ссылки и т.д.
		goldmark.WithRendererOptions(html.WithUnsafe()), // разрешим HTML (для <audio>)
	)

	var out bytes.Buffer
	if err := gm.Convert([]byte(md), &out); err != nil {
		return "", err
	}

	// Обернём простым шаблоном
	htmlDoc := `<!doctype html>
<html><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
 body{font-family:-apple-system,Segoe UI,Roboto,Arial,sans-serif;line-height:1.5;padding:16px;max-width:800px;margin:0 auto}
 img{max-width:100%;height:auto;border-radius:8px;border:1px solid #ddd}
 pre,code{background:#f6f8fa}
 audio{width:100%;margin:8px 0}
</style>
</head><body>` + sanitizeTaskHTML(out.String()) + `</body></html>`

	baseDir, err := appDataDir()
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(baseDir, fmt.Sprintf("task_%d.html", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, []byte(htmlDoc), 0o644); err != nil {
		return "", err
	}
	return tmp, nil
}

func sanitizeTaskHTML(htmlStr string) string {
	p := bluemonday.UGCPolicy()

	// Разрешаем то, что тебе реально нужно:
	// - audio с controls, src
	// - source для audio
	p.AllowElements("audio", "source")
	p.AllowAttrs("controls").OnElements("audio")
	p.AllowAttrs("src").OnElements("audio", "source")
	p.AllowAttrs("type").OnElements("source")

	// Разрешаем img и src (иначе markdown-картинки могут сломаться в некоторых политиках)
	p.AllowElements("img")
	p.AllowAttrs("src", "alt", "title").OnElements("img")

	// Разрешаем file: ссылки для локальных вложений
	p.AllowURLSchemes("http", "https", "mailto", "file")

	return p.Sanitize(htmlStr)
}

func fileURLFromPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	// URL-path всегда со слэшами
	upath := filepath.ToSlash(abs)

	// На Windows нужно /C:/...
	if runtime.GOOS == "windows" {
		if len(upath) >= 2 && upath[1] == ':' {
			upath = "/" + upath
		}
	}

	u := url.URL{
		Scheme: "file",
		Path:   upath,
	}
	return u.String(), nil
}

func pathToFileURL(p string) string {
	p = filepath.Clean(p)
	u := &url.URL{Scheme: "file", Path: p}
	// На Windows нужно, чтобы слеши были /
	return u.String()
}
