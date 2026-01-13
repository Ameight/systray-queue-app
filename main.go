package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	webview "github.com/webview/webview_go"

	"github.com/getlantern/systray"
	"github.com/ncruces/zenity"

	"encoding/base64"
	"net/url"

	"github.com/atotto/clipboard"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

/* =======================
   МОДЕЛИ ДАННЫХ
   ======================= */

type AttachmentType string

const (
	AttachmentNone  AttachmentType = "none"
	AttachmentImage AttachmentType = "image"
	AttachmentAudio AttachmentType = "audio"
)

type Task struct {
	ID             string         `json:"id"`
	Text           string         `json:"text"`
	CreatedAt      time.Time      `json:"created_at"`
	AttachmentPath string         `json:"attachment_path,omitempty"`
	AttachmentType AttachmentType `json:"attachment_type,omitempty"`
}

type taskQueue struct {
	mu             sync.Mutex
	Tasks          []Task `json:"tasks"`
	filePath       string
	attachmentsDir string
}

func newTaskQueue(baseDir string) (*taskQueue, error) {
	q := &taskQueue{
		filePath:       filepath.Join(baseDir, "queue.json"),
		attachmentsDir: filepath.Join(baseDir, "attachments"),
	}
	if err := os.MkdirAll(q.attachmentsDir, 0o755); err != nil {
		return nil, err
	}
	if err := q.loadLocked(); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *taskQueue) loadLocked() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	b, err := os.ReadFile(q.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			q.Tasks = nil
			return nil
		}
		return err
	}
	var tmp struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	q.Tasks = tmp.Tasks
	return nil
}

func (q *taskQueue) saveLocked() error {
	b, err := json.MarshalIndent(struct {
		Tasks []Task `json:"tasks"`
	}{Tasks: q.Tasks}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(q.filePath, b, 0o644)
}

func (q *taskQueue) enqueue(t Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Tasks = append(q.Tasks, t)
	return q.saveLocked()
}

func (q *taskQueue) getAll() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	res := make([]Task, len(q.Tasks))
	copy(res, q.Tasks)
	return res
}

func (q *taskQueue) peek() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 {
		return Task{}, false
	}
	return q.Tasks[0], true
}

func (q *taskQueue) skip() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) <= 1 {
		return nil
	}
	first := q.Tasks[0]
	q.Tasks = append(q.Tasks[1:], first)
	return q.saveLocked()
}

func (q *taskQueue) complete() (Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 {
		return Task{}, errors.New("queue is empty")
	}
	first := q.Tasks[0]
	q.Tasks = q.Tasks[1:]
	if err := q.saveLocked(); err != nil {
		return Task{}, err
	}
	return first, nil
}

/* =======================
   ПУТИ ДАННЫХ / УТИЛИТЫ
   ======================= */

func appDataDir() (string, error) {
	cfgBase, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cfgBase, "systray-queue-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func openWithSystem(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = io.Copy(d, s)
	return err
}

// Простая монохромная иконка 16x16 (подходит как template на macOS).
func makeTemplateIcon() []byte {
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

/* =======================
   НАСТРОЙКИ (Автозапуск)
   ======================= */

func exePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// macOS LaunchAgents
func macLaunchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.example.systray-queue.plist"), nil
}

func setRunAtLogin(enabled bool) error {
	switch runtime.GOOS {
	case "darwin":
		exe, err := exePath()
		if err != nil {
			return err
		}
		plistPath, err := macLaunchAgentPath()
		if err != nil {
			return err
		}
		if enabled {
			if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
				return err
			}
			content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>com.example.systray-queue</string>
	<key>ProgramArguments</key><array><string>%s</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><false/>
	<key>StandardOutPath</key><string>/tmp/systray-queue.out</string>
	<key>StandardErrorPath</key><string>/tmp/systray-queue.err</string>
</dict></plist>`, exe)
			return os.WriteFile(plistPath, []byte(content), 0o644)
		}
		return os.Remove(plistPath)

	case "windows":
		// HKCU\Software\Microsoft\Windows\CurrentVersion\Run
		exe, err := exePath()
		if err != nil {
			return err
		}
		name := "SystrayQueueApp"
		if enabled {
			return exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", name, "/t", "REG_SZ", "/d", exe, "/f").Run()
		}
		return exec.Command("reg", "delete", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", name, "/f").Run()

	default: // Linux/XDG Autostart
		exe, err := exePath()
		if err != nil {
			return err
		}
		cfg, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		dir := filepath.Join(cfg, "autostart")
		file := filepath.Join(dir, "systray-queue-app.desktop")
		if enabled {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Systray Queue App
Exec=%s
X-GNOME-Autostart-enabled=true
`, exe)
			return os.WriteFile(file, []byte(desktop), 0o644)
		}
		return os.Remove(file)
	}
}

func isRunAtLoginEnabled() bool {
	switch runtime.GOOS {
	case "darwin":
		p, err := macLaunchAgentPath()
		if err != nil {
			return false
		}
		_, err = os.Stat(p)
		return err == nil
	case "windows":
		name := "SystrayQueueApp"
		out, err := exec.Command("reg", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", name).CombinedOutput()
		return err == nil && strings.Contains(strings.ToLower(string(out)), strings.ToLower(name))
	default:
		cfg, err := os.UserConfigDir()
		if err != nil {
			return false
		}
		_, err = os.Stat(filepath.Join(cfg, "autostart", "systray-queue-app.desktop"))
		return err == nil
	}
}

/* =======================
   UI ДИАЛОГИ
   ======================= */

func showAddTaskDialog(q *taskQueue) {
	baseDir, _ := appDataDir()
	attachmentsDir := filepath.Join(baseDir, "attachments")
	_ = os.MkdirAll(attachmentsDir, 0o755)

	var prefill string
	var attachPath string
	var aType AttachmentType = AttachmentNone

	for {
		text, err := zenity.Entry(
			"Введите текст задачи:",
			zenity.Title("Добавить задачу"),
			zenity.OKLabel("Добавить"),
			zenity.CancelLabel("Отмена"),
			zenity.ExtraButton("Вставить из буфера"),
			zenity.EntryText(prefill), // можно подставить текст из буфера
		)

		switch {
		case err == nil:
			text = strings.TrimSpace(text)
			if text == "" {
				_ = zenity.Error("Текст задачи не может быть пустым", zenity.Title("Ошибка"))
				continue
			}
			t := Task{
				ID:             fmt.Sprintf("tsk_%d", time.Now().UnixNano()),
				Text:           text,
				CreatedAt:      time.Now(),
				AttachmentPath: attachPath,
				AttachmentType: aType,
			}
			if err := q.enqueue(t); err != nil {
				_ = zenity.Error(fmt.Sprintf("Не удалось добавить задачу: %v", err), zenity.Title("Ошибка"))
			} else {
				_ = zenity.Info("Задача добавлена", zenity.Title("Готово"))
			}
			return

		case errors.Is(err, zenity.ErrExtraButton):
			// нажали «Вставить из буфера»
			// внутри case errors.Is(err, zenity.ErrExtraButton) { ... }
			p, at, txt, clipErr := tryClipboard(attachmentsDir)
			if clipErr != nil {
				_ = zenity.Error("В буфере нет подходящего изображения/файла/текста.", zenity.Title("Буфер обмена"))
				continue
			}
			if p != "" && at == AttachmentImage {
				// вставим MD-картинку в текст
				imgMD := fmt.Sprintf("\n\n![screenshot](%s)\n", pathToFileURL(p))
				if prefill == "" {
					prefill = imgMD
				} else {
					prefill = prefill + imgMD
				}
				// при этом можно НЕ сохранять attachPath отдельно — картинка уже в тексте
				// если хочешь оставить старую семантику «ещё и AttachmentPath», можешь:
				// attachPath, aType = p, at
			} else if p != "" && at == AttachmentAudio {
				// аудио можно тоже подсунуть в MD ссылкой, а плеер добавим при рендере (см. renderMarkdownToTempHTML)
				attachPath, aType = p, at // пригодится для <audio>
			}
			if txt != "" {
				// текст из буфера просто подставляем
				if prefill == "" {
					prefill = txt
				} else {
					prefill = prefill + "\n" + txt
				}
			}

		case errors.Is(err, zenity.ErrCanceled):
			return

		default:
			_ = zenity.Error(fmt.Sprintf("Ошибка диалога: %v", err), zenity.Title("Ошибка"))
			return
		}
	}

}

func showFirstTaskDialogOld(q *taskQueue) {
	t, ok := q.peek()
	if !ok {
		_ = zenity.Info("Очередь пуста", zenity.Title("Задачи"))
		return
	}

	audio := ""
	if t.AttachmentType == AttachmentAudio && t.AttachmentPath != "" {
		audio = t.AttachmentPath
	}
	htmlPath, err := renderMarkdownToTempHTML(t.Text, audio)
	if err != nil {
		_ = zenity.Error(fmt.Sprintf("Ошибка рендера Markdown: %v", err), zenity.Title("Ошибка"))
		return
	}

	// webview обязательно в main thread!
	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle("Первая задача")
	w.SetSize(800, 600, webview.HintNone)
	w.Navigate("file://" + htmlPath)
	w.Run()
}

func showFirstTaskDialog(q *taskQueue) {
	t, ok := q.peek()
	if !ok {
		_ = zenity.Info("Очередь пуста", zenity.Title("Задачи"))
		return
	}

	// t.Text — теперь MD. Рендерим в HTML и открываем системным браузером.
	audio := ""
	if t.AttachmentType == AttachmentAudio && t.AttachmentPath != "" {
		audio = t.AttachmentPath
	}
	htmlPath, err := renderMarkdownToTempHTML(t.Text, audio)
	if err != nil {
		_ = zenity.Error(fmt.Sprintf("Ошибка рендера Markdown: %v", err), zenity.Title("Ошибка"))
		return
	}
	_ = openWithSystem(htmlPath)
}

/* =======================
   ТРЕЙ
   ======================= */

func onReady() {
	baseDir, err := appDataDir()
	if err != nil {
		log.Fatal(err)
	}
	q, err := newTaskQueue(baseDir)
	if err != nil {
		log.Fatal(err)
	}

	icon := makeTemplateIcon()
	systray.SetTemplateIcon(icon, icon)
	systray.SetTitle("Tasks")
	systray.SetTooltip("Очередь задач")

	// Основные пункты
	mAdd := systray.AddMenuItem("Добавить задачу", "Добавить новую задачу")
	mShow := systray.AddMenuItem("Получить первую задачу", "Показать первую задачу")
	mSkip := systray.AddMenuItem("Пропустить задачу", "Переместить первую задачу в конец")
	mDone := systray.AddMenuItem("Завершить задачу", "Удалить первую задачу")
	mList := systray.AddMenuItem("Все задачи…", "Посмотреть и изменить порядок")

	// Настройки
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Настройки", "Параметры приложения (автозапуск, конфиг)")
	mAuto := mSettings.AddSubMenuItemCheckbox("Запускаться при входе", "Автозапуск при входе в систему", isRunAtLoginEnabled())
	mOpenCfg := mSettings.AddSubMenuItem("Открыть папку конфига", "Открыть каталог с queue.json и attachments")

	// Выход
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Завершить приложение")

	updateTooltip := func() {
		q.mu.Lock()
		n := len(q.Tasks)
		q.mu.Unlock()
		systray.SetTooltip(fmt.Sprintf("Очередь задач — %d", n))
	}
	updateTooltip()

	go func() {
		for {
			select {
			case <-mAdd.ClickedCh:
				showAddTaskDialog(q)
				updateTooltip()
			case <-mShow.ClickedCh:
				showFirstTaskDialog(q)
			case <-mSkip.ClickedCh:
				if err := q.skip(); err != nil {
					_ = zenity.Error(err.Error(), zenity.Title("Ошибка"))
				}
				updateTooltip()
			case <-mDone.ClickedCh:
				if _, err := q.complete(); err != nil {
					_ = zenity.Error(err.Error(), zenity.Title("Ошибка"))
				}
				updateTooltip()
			case <-mAuto.ClickedCh:
				want := !mAuto.Checked()
				if err := setRunAtLogin(want); err != nil {
					_ = zenity.Error(fmt.Sprintf("Не удалось изменить автозапуск: %v", err), zenity.Title("Настройки"))
				} else {
					if want {
						mAuto.Check()
					} else {
						mAuto.Uncheck()
					}
				}
			case <-mOpenCfg.ClickedCh:
				_ = openWithSystem(baseDir)
			case <-mList.ClickedCh:
				showAllTasksDialog(q)
				// по желанию: обновить тултип (кол-во не меняется, но порядок — да)
				// updateTooltip()

			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {}

// Пытается достать вложение или текст из буфера обмена.
// Возвращает: attachmentPath, attachmentType, prefillText, error.
// Если найдено изображение/файл — вернёт путь к локальной копии в attachments/.
// Если найден текст — вернёт prefillText для поля ввода.
func tryClipboard(attachmentsDir string) (string, AttachmentType, string, error) {
	// 1) macOS: попробуем pngpaste (если установлен) — достаёт PNG из буфера
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("pngpaste"); err == nil {
			if out, err := exec.Command("pngpaste", "-").Output(); err == nil && len(out) > 0 {
				name := fmt.Sprintf("%d_clipboard.png", time.Now().UnixNano())
				dst := filepath.Join(attachmentsDir, name)
				if werr := os.WriteFile(dst, out, 0o644); werr == nil {
					return dst, AttachmentImage, "", nil
				}
			}
		}
	}

	// 2) Текст из буфера
	txt, _ := clipboard.ReadAll()
	txt = strings.TrimSpace(txt)
	if txt == "" {
		return "", AttachmentNone, "", errors.New("clipboard is empty")
	}

	// data URL (например, data:image/png;base64,...)
	if strings.HasPrefix(strings.ToLower(txt), "data:") {
		if p, at, ok := decodeDataURLToFile(txt, attachmentsDir); ok {
			return p, at, "", nil
		}
		return "", AttachmentNone, txt, nil // не распознали как файл — считаем текстом
	}

	// Путь к локальному файлу?
	if looksLikePath(txt) && fileExists(txt) {
		base := fmt.Sprintf("%d_%s", time.Now().UnixNano(), filepath.Base(txt))
		dst := filepath.Join(attachmentsDir, base)
		if err := copyFile(txt, dst); err == nil {
			ext := strings.ToLower(filepath.Ext(dst))
			switch ext {
			case ".png", ".jpg", ".jpeg":
				return dst, AttachmentImage, "", nil
			case ".m4a", ".mp3":
				return dst, AttachmentAudio, "", nil
			default:
				return dst, AttachmentNone, "", nil
			}
		}
		// если копия не удалась — хотя бы вернём исходный текст
		return "", AttachmentNone, txt, nil
	}

	// Иначе — это текст задачи
	return "", AttachmentNone, txt, nil
}

func looksLikePath(s string) bool {
	if runtime.GOOS == "windows" {
		// C:\... или \\server\share\...
		return (len(s) > 2 && s[1] == ':' && (s[2] == '\\' || s[2] == '/')) || strings.HasPrefix(s, `\\`)
	}
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "~")
}

func fileExists(p string) bool {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func decodeDataURLToFile(dataURL, attachmentsDir string) (string, AttachmentType, bool) {
	u, err := url.Parse(dataURL)
	if err != nil || u.Scheme != "data" {
		return "", AttachmentNone, false
	}
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return "", AttachmentNone, false
	}
	meta := parts[0] // e.g. "data:image/png;base64"
	raw := parts[1]

	if !strings.Contains(meta, ";base64") {
		return "", AttachmentNone, false // для простоты поддерживаем только base64
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(dec) == 0 {
		return "", AttachmentNone, false
	}

	// медиа-тип → расширение/тип
	media := strings.TrimPrefix(strings.Split(meta, ";")[0], "data:")
	ext := "bin"
	at := AttachmentNone
	switch {
	case strings.HasPrefix(media, "image/png"):
		ext, at = "png", AttachmentImage
	case strings.HasPrefix(media, "image/jpeg"):
		ext, at = "jpg", AttachmentImage
	case strings.HasPrefix(media, "audio/mpeg"):
		ext, at = "mp3", AttachmentAudio
	case strings.HasPrefix(media, "audio/mp4"), strings.HasPrefix(media, "audio/x-m4a"):
		ext, at = "m4a", AttachmentAudio
	}

	name := fmt.Sprintf("%d_clipboard.%s", time.Now().UnixNano(), ext)
	dst := filepath.Join(attachmentsDir, name)
	if err := os.WriteFile(dst, dec, 0o644); err != nil {
		return "", AttachmentNone, false
	}
	return dst, at, true
}

func main() {
	// macOS: systray.Run должен быть вызван на главном OS-потоке
	runtime.LockOSThread()
	systray.Run(onReady, onExit)
}

// file:// URL для локального пути (нужно, чтобы <img src="..."> работало в браузере).
func pathToFileURL(p string) string {
	p = filepath.Clean(p)
	u := &url.URL{Scheme: "file", Path: p}
	// На Windows нужно, чтобы слеши были /
	return u.String()
}

// Рендерим Markdown -> HTML и сохраняем во временный .html рядом с данными приложения
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
</head><body>` + out.String() + `</body></html>`

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

// move перемещает элемент с позиции from на позицию to (индексы 0-based).
func (q *taskQueue) move(from, to int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.Tasks)
	if n == 0 {
		return errors.New("queue is empty")
	}
	if from < 0 || from >= n || to < 0 || to >= n {
		return fmt.Errorf("indexes out of range: from=%d to=%d (0..%d)", from, to, n-1)
	}
	if from == to {
		return nil
	}
	item := q.Tasks[from]
	// удаляем from
	q.Tasks = append(q.Tasks[:from], q.Tasks[from+1:]...)
	// вставляем в to
	if to >= len(q.Tasks) {
		q.Tasks = append(q.Tasks, item)
	} else {
		q.Tasks = append(q.Tasks[:to], append([]Task{item}, q.Tasks[to:]...)...)
	}
	return q.saveLocked()
}

func summarizeTask(t Task) string {
	att := ""
	switch t.AttachmentType {
	case AttachmentImage:
		att = " [img]"
	case AttachmentAudio:
		att = " [audio]"
	}
	txt := t.Text
	if len(txt) > 60 {
		txt = txt[:57] + "…"
	}
	return fmt.Sprintf("%s %s%s", t.CreatedAt.Format("2006-01-02 15:04"), txt, att)
}

func renderQueueList(q *taskQueue) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 {
		return "(пусто)"
	}
	var b strings.Builder
	for i, t := range q.Tasks {
		fmt.Fprintf(&b, "%2d. %s\n", i+1, summarizeTask(t))
	}
	return b.String()
}

// Показать весь список + кнопка "Редактировать порядок"
func showAllTasksDialog(q *taskQueue) {
	list := renderQueueList(q)
	err := zenity.Question(
		list,
		zenity.Title("Все задачи"),
		zenity.OKLabel("Закрыть"),
		zenity.ExtraButton("Редактировать порядок"),
	)
	if errors.Is(err, zenity.ErrExtraButton) {
		reorderTasksDialog(q)
	}
	// OK / Cancel — просто закрыть
}

// Цикл редактирования: пользователь вводит перемещения вида "5 1" или "5->1"
func reorderTasksDialog(q *taskQueue) {
	for {
		list := renderQueueList(q)
		prompt := "Текущий порядок:\n\n" + list +
			"\nВведите перемещение в формате \"от куда куда\" (например: 5 1 или 5->1).\nПустая строка или Отмена — выйти."
		input, err := zenity.Entry(
			prompt,
			zenity.Title("Редактировать порядок"),
			zenity.OKLabel("Переместить"),
			zenity.CancelLabel("Готово"),
		)
		if errors.Is(err, zenity.ErrCanceled) {
			return
		}
		trim := strings.TrimSpace(input)
		if trim == "" {
			return
		}
		// Парсим "a b" или "a->b"
		from, to, parseErr := parseMove(trim)
		if parseErr != nil {
			_ = zenity.Error("Формат: \"5 1\" или \"5->1\"", zenity.Title("Ошибка ввода"))
			continue
		}
		// В UI индексы 1-based, в очереди 0-based
		if err := q.move(from-1, to-1); err != nil {
			_ = zenity.Error(err.Error(), zenity.Title("Ошибка перемещения"))
			continue
		}
	}
}

func parseMove(s string) (int, int, error) {
	s = strings.ReplaceAll(s, "->", " ")
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("bad format")
	}
	a, err1 := strconv.Atoi(fields[0])
	b, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("bad numbers")
	}
	if a <= 0 || b <= 0 {
		return 0, 0, fmt.Errorf("indexes must be >= 1")
	}
	return a, b, nil
}
