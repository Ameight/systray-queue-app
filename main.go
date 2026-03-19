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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/microcosm-cc/bluemonday"
	webview "github.com/webview/webview_go"
	"golang.design/x/hotkey"
	"gopkg.in/yaml.v3"

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
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(q.filePath, data, 0644)
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
		return Task{}, nil
	}

	task := q.Tasks[0]
	q.Tasks = q.Tasks[1:]

	if err := q.saveLocked(); err != nil {
		return Task{}, err
	}

	// Удаляем вложение после успешного сохранения очереди
	if task.AttachmentPath != "" && q.attachmentsDir != "" {
		inside, err := isPathInsideDir(task.AttachmentPath, q.attachmentsDir)
		if err == nil && inside {
			_ = os.Remove(task.AttachmentPath) // мягко: не валим операцию, если файла уже нет
		}
	}

	return task, nil
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

	keyCfgPath := filepath.Join(baseDir, "key-config.yaml")
	keyCfg, err := loadOrCreateKeyConfig(keyCfgPath)
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
	mManage := systray.AddMenuItem("Поменять порядок…", "Поменять порядок drag & drop")

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
			case <-mManage.ClickedCh:
				manageOnce.Do(func() {
					manageURL, manageErr = startManageServer(q)
				})
				if manageErr != nil {
					_ = zenity.Error(fmt.Sprintf("Failed to start manage UI: %v", manageErr), zenity.Title("Error"))
					break
				}
				_ = openBrowser(manageURL)

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

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// На всякий случай: если что-то пойдёт не так — уберём tmp
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Rename поверх целевого файла
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	// Максимальная надёжность: sync директории (актуально на Unix)
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
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

func isPathInsideDir(filePath, dir string) (bool, error) {
	absFile, err := filepath.Abs(filePath)
	if err != nil {
		return false, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}

	rel, err := filepath.Rel(absDir, absFile)
	if err != nil {
		return false, err
	}

	// rel не должен начинаться с ".." и не должен быть абсолютным
	if rel == "." {
		// файл = директория — нам не подходит
		return false, nil
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.IsAbs(rel) {
		return false, nil
	}
	return true, nil
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

func (q *taskQueue) reorderByIndicesLocked(order []int) error {
	n := len(q.Tasks)
	if len(order) != n {
		return fmt.Errorf("bad order length: got %d want %d", len(order), n)
	}

	seen := make([]bool, n)
	newTasks := make([]Task, n)

	for i, idx := range order {
		if idx < 0 || idx >= n {
			return fmt.Errorf("index out of range: %d", idx)
		}
		if seen[idx] {
			return fmt.Errorf("duplicate index: %d", idx)
		}
		seen[idx] = true
		newTasks[i] = q.Tasks[idx]
	}

	q.Tasks = newTasks
	return q.saveLocked()
}

func (q *taskQueue) reorderByIndices(order []int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reorderByIndicesLocked(order)
}

var (
	manageOnce sync.Once
	manageURL  string
	manageErr  error
)

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func startManageServer(q *taskQueue) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // случайный свободный порт
	if err != nil {
		return "", err
	}

	mux := http.NewServeMux()

	// HTML страница
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Берём снэпшот задач под lock
		q.mu.Lock()
		tasks := make([]Task, len(q.Tasks))
		copy(tasks, q.Tasks)
		q.mu.Unlock()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, renderManageHTML(tasks))
	})

	// Принять новый порядок
	mux.HandleFunc("/reorder", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Order []int `json:"order"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		// Минимальная валидация (сортировка копии для проверки permutation)
		ord := append([]int(nil), req.Order...)
		sort.Ints(ord)
		for i := range ord {
			if ord[i] != i {
				http.Error(w, "bad permutation", http.StatusBadRequest)
				return
			}
		}

		if err := q.reorderByIndices(req.Order); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, `{"ok":true}`)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		_ = srv.Serve(ln)
	}()

	url := "http://" + ln.Addr().String() + "/"
	return url, nil
}

func renderManageHTML(tasks []Task) string {
	// ВАЖНО: мы не вставляем сырой task.Text в HTML напрямую (там может быть что угодно).
	// Для простоты показываем короткий превью-текст в текстовом узле через JS-эскейп? —
	// Тут проще: рендерим сервером только data-idx, а текст экранируем очень грубо.
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "&", "&amp;")
		s = strings.ReplaceAll(s, "<", "&lt;")
		s = strings.ReplaceAll(s, ">", "&gt;")
		s = strings.ReplaceAll(s, `"`, "&quot;")
		return s
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>Manage queue</title>`)
	b.WriteString(`<style>
        body{font-family:system-ui,-apple-system,Segoe UI,Roboto,Ubuntu,Cantarell,Noto Sans,sans-serif;margin:20px;max-width:900px}
        h1{font-size:20px;margin:0 0 12px}
        .row{display:flex;gap:10px;align-items:center;margin:12px 0}
        button{padding:8px 12px;border-radius:8px;border:1px solid #ccc;background:#fff;cursor:pointer}
        button:hover{background:#f5f5f5}
        #status{font-size:12px;color:#666}
        ul{list-style:none;padding:0;margin:0;border:1px solid #ddd;border-radius:12px;overflow:hidden}
        li{padding:10px 12px;border-bottom:1px solid #eee;cursor:grab;background:#fff}
        li:last-child{border-bottom:none}
        li.dragging{opacity:.5}
        li.over{outline:2px dashed #999;outline-offset:-2px}
        .hint{font-size:12px;color:#666;margin-top:10px}
    </style></head><body>`)
	b.WriteString(`<h1>Manage queue</h1>`)
	b.WriteString(`<div class="row"><button id="save">Save</button><span id="status"></span></div>`)
	b.WriteString(`<ul id="list">`)

	for i, t := range tasks {
		// показываем превью первой строки
		prev := t.Text
		if idx := strings.IndexByte(prev, '\n'); idx >= 0 {
			prev = prev[:idx]
		}
		if len(prev) > 140 {
			prev = prev[:140] + "…"
		}
		b.WriteString(fmt.Sprintf(`<li draggable="true" data-idx="%d">%d. %s</li>`, i, i+1, esc(prev)))
	}

	b.WriteString(`</ul>`)
	b.WriteString(`<div class="hint">Drag tasks to reorder. Click Save to persist.</div>`)

	b.WriteString(`<script>
        const list = document.getElementById('list');
        const status = document.getElementById('status');
        let dragging = null;

        function setStatus(msg){ status.textContent = msg || ''; }

        list.addEventListener('dragstart', (e) => {
            const li = e.target.closest('li');
            if (!li) return;
            dragging = li;
            li.classList.add('dragging');
            e.dataTransfer.effectAllowed = 'move';
            // для Firefox нужно data
            e.dataTransfer.setData('text/plain', li.dataset.idx);
        });

        list.addEventListener('dragend', (e) => {
            const li = e.target.closest('li');
            if (!li) return;
            li.classList.remove('dragging');
            [...list.querySelectorAll('li.over')].forEach(x => x.classList.remove('over'));
            dragging = null;
        });

        list.addEventListener('dragover', (e) => {
            e.preventDefault();
            const over = e.target.closest('li');
            if (!over || !dragging || over === dragging) return;
            over.classList.add('over');

            const rect = over.getBoundingClientRect();
            const before = (e.clientY - rect.top) < rect.height / 2;
            if (before) {
                list.insertBefore(dragging, over);
            } else {
                list.insertBefore(dragging, over.nextSibling);
            }
        });

        list.addEventListener('dragleave', (e) => {
            const li = e.target.closest('li');
            if (li) li.classList.remove('over');
        });

        document.getElementById('save').addEventListener('click', async () => {
            const order = [...list.querySelectorAll('li')].map(li => parseInt(li.dataset.idx, 10));
            setStatus('Saving…');
            try {
                const res = await fetch('/reorder', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({order})
                });
                if (!res.ok) throw new Error(await res.text());
                setStatus('Saved');
                setTimeout(()=>setStatus(''), 1200);
            } catch (err) {
                setStatus('Error: ' + err.message);
            }
        });
    </script>`)

	b.WriteString(`</body></html>`)
	return b.String()
}

type HotkeyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Combo   string `yaml:"combo"`
}

type KeyConfig struct {
	Version int                     `yaml:"version"`
	Hotkeys map[string]HotkeyConfig `yaml:"hotkeys"`
}

func defaultKeyConfig() KeyConfig {
	return KeyConfig{
		Version: 1,
		Hotkeys: map[string]HotkeyConfig{
			"show_first":         {Enabled: true, Combo: "ctrl+alt+q"},
			"add_from_clipboard": {Enabled: true, Combo: "ctrl+alt+a"},
			"skip":               {Enabled: true, Combo: "ctrl+alt+s"},
			"complete":           {Enabled: true, Combo: "ctrl+alt+d"},
			"manage_queue":       {Enabled: true, Combo: "ctrl+alt+m"},
		},
	}
}

func loadOrCreateKeyConfig(path string) (KeyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := defaultKeyConfig()
			out, _ := yaml.Marshal(cfg)
			if err := atomicWriteFile(path, out, 0644); err != nil {
				return KeyConfig{}, err
			}
			return cfg, nil
		}
		return KeyConfig{}, err
	}

	var cfg KeyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return KeyConfig{}, fmt.Errorf("failed to parse key-config.yaml: %w", err)
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Hotkeys == nil {
		cfg.Hotkeys = map[string]HotkeyConfig{}
	}
	return cfg, nil
}

func parseHotkeyCombo(combo string) ([]hotkey.Modifier, hotkey.Key, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(combo)), "+")
	if len(parts) == 0 {
		return nil, 0, fmt.Errorf("empty combo")
	}

	var mods []hotkey.Modifier
	// Последний токен считаем "основной клавишей"
	keyToken := strings.TrimSpace(parts[len(parts)-1])
	modTokens := parts[:len(parts)-1]

	for _, mt := range modTokens {
		mt = strings.TrimSpace(mt)
		switch mt {
		case "ctrl", "control":
			mods = append(mods, hotkey.ModCtrl)
		case "alt", "option":
			mods = append(mods, hotkey.ModAlt)
		case "shift":
			mods = append(mods, hotkey.ModShift)
		case "cmd", "command", "meta", "super", "win":
			mods = append(mods, hotkey.ModCmd)
		default:
			return nil, 0, fmt.Errorf("unknown modifier: %s", mt)
		}
	}

	// Клавиша
	if k, ok := parseKeyToken(keyToken); ok {
		return mods, k, nil
	}
	return nil, 0, fmt.Errorf("unknown key: %s", keyToken)
}

func parseKeyToken(t string) (hotkey.Key, bool) {
	// Буквы
	if len(t) == 1 && t[0] >= 'a' && t[0] <= 'z' {
		return hotkey.Key(t[0] - 'a' + byte(hotkey.KeyA)), true
	}
	// Цифры
	if len(t) == 1 && t[0] >= '0' && t[0] <= '9' {
		// В hotkey обычно есть Key0..Key9, но на всякий — можно расширить позже.
		switch t[0] {
		case '0':
			return hotkey.Key0, true
		case '1':
			return hotkey.Key1, true
		case '2':
			return hotkey.Key2, true
		case '3':
			return hotkey.Key3, true
		case '4':
			return hotkey.Key4, true
		case '5':
			return hotkey.Key5, true
		case '6':
			return hotkey.Key6, true
		case '7':
			return hotkey.Key7, true
		case '8':
			return hotkey.Key8, true
		case '9':
			return hotkey.Key9, true
		}
	}

	switch t {
	case "space":
		return hotkey.KeySpace, true
	case "enter", "return":
		return hotkey.KeyReturn, true
	case "tab":
		return hotkey.KeyTab, true
	case "esc", "escape":
		return hotkey.KeyEscape, true
	}

	// F1..F12
	if strings.HasPrefix(t, "f") {
		n, err := strconv.Atoi(strings.TrimPrefix(t, "f"))
		if err == nil {
			switch n {
			case 1:
				return hotkey.KeyF1, true
			case 2:
				return hotkey.KeyF2, true
			case 3:
				return hotkey.KeyF3, true
			case 4:
				return hotkey.KeyF4, true
			case 5:
				return hotkey.KeyF5, true
			case 6:
				return hotkey.KeyF6, true
			case 7:
				return hotkey.KeyF7, true
			case 8:
				return hotkey.KeyF8, true
			case 9:
				return hotkey.KeyF9, true
			case 10:
				return hotkey.KeyF10, true
			case 11:
				return hotkey.KeyF11, true
			case 12:
				return hotkey.KeyF12, true
			}
		}
	}

	return 0, false
}

type registeredHotkey struct {
	Action string
	HK     *hotkey.Hotkey
}

func registerHotkeys(cfg KeyConfig, actionFn map[string]func()) ([]registeredHotkey, error) {
	var regs []registeredHotkey

	for action, hc := range cfg.Hotkeys {
		if !hc.Enabled {
			continue
		}
		fn, ok := actionFn[action]
		if !ok {
			// неизвестный action — пропускаем (или можно вернуть ошибку)
			continue
		}

		mods, key, err := parseHotkeyCombo(hc.Combo)
		if err != nil {
			return nil, fmt.Errorf("hotkey %s (%q): %w", action, hc.Combo, err)
		}

		hk := hotkey.New(mods, key)
		if err := hk.Register(); err != nil {
			return nil, fmt.Errorf("failed to register hotkey %s (%q): %w", action, hc.Combo, err)
		}

		regs = append(regs, registeredHotkey{Action: action, HK: hk})

		// слушаем нажатия
		go func(fn func(), hk *hotkey.Hotkey) {
			for range hk.Keydown() {
				fn()
			}
		}(fn, hk)
	}

	return regs, nil
}

func unregisterHotkeys(regs []registeredHotkey) {
	for _, r := range regs {
		_ = r.HK.Unregister()
	}
}
