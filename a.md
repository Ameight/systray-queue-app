# systray-queue-app

Кроссплатформенное приложение на Go 1.22+ с системным треем (github.com/getlantern/systray) и локальной очередью задач с вложениями (изображение/аудио).

---

## 📁 Структура

```
systray-queue-app/
├─ go.mod
├─ main.go
├─ README.md
└─ macos/Info.plist        # для упаковки в .app (LSUIElement=1)
```

> Иконки: приложение работает и без иконок (будет заголовок/тултип). При желании добавьте свои монохромные PNG/ICO и раскомментируйте строки `SetTemplateIcon`/`SetIcon`.

---

## go.mod

```mod
module example.com/systray-queue-app

go 1.22

require (
	github.com/getlantern/systray v1.2.1 // или новее
	github.com/ncruces/zenity v0.10.9   // нативные системные диалоги
	github.com/webview/webview_go_go v0.1.1    // мини-диалог предпросмотра (HTML)
)
```

---

## main.go

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"github.com/ncruces/zenity"
	webview "github.com/webview/webview_go_go"
)

// ====== МОДЕЛИ ДАННЫХ ======

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
	mu    sync.Mutex
	Tasks []Task `json:"tasks"`

	filePath      string
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
	// Загрузка существующей очереди, если есть
	_ = q.load()
	return q, nil
}

// saveLocked выполняет запись JSON на диск. Вызывать ТОЛЬКО под захваченным q.mu.
func (q *taskQueue) saveLocked() error {
	b, err := json.MarshalIndent(struct{ Tasks []Task `json:"tasks"` }{Tasks: q.Tasks}, "", "  ")
	if err != nil { return err }
	return os.WriteFile(q.filePath, b, 0o644)
}

func (q *taskQueue) load() error {
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
	return json.Unmarshal(b, q)
}

func (q *taskQueue) enqueue(t Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Tasks = append(q.Tasks, t)
	return q.saveLocked()
}

func (q *taskQueue) peek() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 { return Task{}, false }
	return q.Tasks[0], true
}

func (q *taskQueue) skip() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) <= 1 { return nil }
	first := q.Tasks[0]
	q.Tasks = append(q.Tasks[1:], first)
	return q.saveLocked()
}

func (q *taskQueue) complete() (Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 { return Task{}, errors.New("queue is empty") }
	first := q.Tasks[0]
	q.Tasks = q.Tasks[1:]
	if err := q.saveLocked(); err != nil { return Task{}, err }
	return first, nil
}

// ====== ПУТИ ДАННЫХ ======

func appDataDir() (string, error) {
	// ~/.local/share/appname (Linux), ~/Library/Application Support/appname (macOS), %AppData%\\appname (Windows)
	cfgBase, err := os.UserConfigDir()
	if err != nil { return "", err }
	dir := filepath.Join(cfgBase, "systray-queue-app")
	if err := os.MkdirAll(dir, 0o755); err != nil { return "", err }
	return dir, nil
}

// ====== UI ДИАЛОГИ ======

func showAddTaskDialog(q *taskQueue) {
	// 1) Ввод текста задачи
	text, err := zenity.Entry(
		"Введите текст задачи:",
		zenity.Title("Добавить задачу"),
		zenity.OKLabel("Далее"),
		zenity.CancelLabel("Отмена"),
	)
	if err != nil { // отмена
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		_ = zenity.Error("Текст задачи не может быть пустым", zenity.Title("Ошибка"))
		return
	}

	// 2) Выбор вложения (необязательно)
	var attachPath string
	var aType AttachmentType = AttachmentNone
	if err := zenity.Question(
		"Хотите прикрепить файл? (PNG/JPG/M4A/MP3)",
		zenity.Title("Вложение"),
		zenity.OKLabel("Да"), zenity.CancelLabel("Нет"),
	); err == nil {
		filters := []zenity.FileFilter{
			{Name: "Изображения (PNG/JPG)", Patterns: []string{"*.png", "*.jpg", "*.jpeg"}},
			{Name: "Аудио (M4A/MP3)", Patterns: []string{"*.m4a", "*.mp3"}},
		}
		fp, ferr := zenity.SelectFile(
			zenity.Title("Выберите файл"),
			zenity.FileFilters(filters...),
		)
		if ferr == nil && fp != "" {
			attachPath = fp
			ext := strings.ToLower(filepath.Ext(fp))
			if ext == ".png" || ext == ".jpg" || ext == ".jpeg" { aType = AttachmentImage }
			if ext == ".m4a" || ext == ".mp3" { aType = AttachmentAudio }
		}
	}

	// 3) Копируем вложение в каталог приложения
	var storedPath string
	if attachPath != "" {
		base := fmt.Sprintf("%d_%s", time.Now().UnixNano(), filepath.Base(attachPath))
		dst := filepath.Join(q.attachmentsDir, base)
		if err := copyFile(attachPath, dst); err != nil {
			_ = zenity.Error(fmt.Sprintf("Не удалось сохранить вложение: %v", err), zenity.Title("Ошибка"))
			return
		}
		storedPath = dst
	}

	// 4) Сохраняем задачу
	t := Task{
		ID:             fmt.Sprintf("tsk_%d", time.Now().UnixNano()),
		Text:           text,
		CreatedAt:      time.Now(),
		AttachmentPath: storedPath,
		AttachmentType: aType,
	}
	if err := q.enqueue(t); err != nil {
		_ = zenity.Error(fmt.Sprintf("Не удалось добавить задачу: %v", err), zenity.Title("Ошибка"))
		return
	}
	_ = zenity.Info("Задача добавлена в очередь", zenity.Title("Готово"))
}

func showFirstTaskDialog(q *taskQueue) {
	t, ok := q.peek()
	if !ok {
		_ = zenity.Info("Очередь пуста", zenity.Title("Задачи"))
		return
	}

	// Рендерим мини-диалог в webview (только чтение + предпросмотр)
	html := buildTaskHTML(t)
	w := webview.New(true)
	defer w.Destroy()
	w.SetTitle("Первая задача")
	w.SetSize(520, 420, webview.HintNone)
	w.Navigate("data:text/html," + urlEncodeHTML(html))
	w.Run()
}

func buildTaskHTML(t Task) string {
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<style>body{font-family:-apple-system,Segoe UI,Roboto,Arial,sans-serif;padding:16px;line-height:1.45} .box{border:1px solid #ddd;border-radius:12px;padding:12px} .muted{color:#666;font-size:12px} img{max-width:100%;height:auto;border-radius:8px;border:1px solid #ccc} audio{width:100%;margin-top:8px}</style></head><body>")
	b.WriteString("<h3>Первая задача</h3>")
	b.WriteString("<div class=box>")
	b.WriteString("<div class=muted>" + t.CreatedAt.Format("2006-01-02 15:04:05") + "</div>")
	b.WriteString("<p>" + htmlEscape(t.Text) + "</p>")
	if t.AttachmentPath != "" {
		p := pathToFileURL(t.AttachmentPath)
		switch t.AttachmentType {
		case AttachmentImage:
			b.WriteString("<img src=\"" + p + "\" alt=\"attachment\">")
		case AttachmentAudio:
			b.WriteString("<audio controls src=\"" + p + "\"></audio>")
		}
	}
	b.WriteString("</div>")
	b.WriteString("<p class=muted>Закройте окно, чтобы вернуться в меню трея.\nИспользуйте пункты меню \"Пропустить\" или \"Завершить\" для управления очередью.</p>")
	b.WriteString("</body></html>")
	return b.String()
}

func pathToFileURL(p string) string {
	p = filepath.ToSlash(p)
	if strings.HasPrefix(p, "/") {
		return "file://" + p
	}
	// Windows
	if len(p) >= 2 && p[1] == ':' {
		return "file:///" + p
	}
	return p
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}

func urlEncodeHTML(s string) string {
	// Простая percent-encode для data: URL
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("-_.~:/?&=;,+#% ", rune(c)) {
			if c == ' ' { b.WriteString("%20") } else { b.WriteByte(c) }
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil { return err }
	defer s.Close()
	d, err := os.Create(dst)
	if err != nil { return err }
	defer d.Close()
	_, err = io.Copy(d, s)
	return err
}

// ====== ТРЕЙ ======

// uiDispatch — канал для выполнения UI-операций на главном OS-потоке (macOS требует этого для NSWindow/WebView).
var uiDispatch chan func()

func onReady() {
	// Инициализация данных
	baseDir, err := appDataDir()
	if err != nil { log.Fatal(err) }
	q, err := newTaskQueue(baseDir)
	if err != nil { log.Fatal(err) }

	// systray UI
	// systray.SetTemplateIcon(iconTemplatePNG, iconTemplatePNG)
	systray.SetTitle("Tasks")
	systray.SetTooltip("Очередь задач")

	mAdd := systray.AddMenuItem("Добавить задачу", "Добавить новую задачу")
	mShow := systray.AddMenuItem("Получить первую задачу", "Показать первую задачу")
	mSkip := systray.AddMenuItem("Пропустить задачу", "Переместить первую задачу в конец")
	mDone := systray.AddMenuItem("Завершить задачу", "Удалить первую задачу")
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
				uiDispatch <- func() { showAddTaskDialog(q); updateTooltip() }
			case <-mShow.ClickedCh:
				uiDispatch <- func() { showFirstTaskDialog(q) }
			case <-mSkip.ClickedCh:
				if err := q.skip(); err != nil { _ = zenity.Error(err.Error(), zenity.Title("Ошибка")) }
				updateTooltip()
			case <-mDone.ClickedCh:
				if _, err := q.complete(); err != nil { _ = zenity.Error(err.Error(), zenity.Title("Ошибка")) }
				updateTooltip()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}


func onExit() {
	// Освобождение ресурсов при выходе, если нужно
}

func main() {
	// macOS: UI должен создаваться на главном OS-потоке
	runtime.LockOSThread()

	uiDispatch = make(chan func())

	// Запускаем systray на отдельной горутине (он держит свой цикл/поток)
	go systray.Run(onReady, onExit)

	// Главный OS-поток — диспетчер UI-задач (webview, NSWindow и т.п.)
	for fn := range uiDispatch {
		if fn != nil { fn() }
	}
}

```

---

## README.md (сборка и упаковка)

````md
# systray-queue-app

## Сборка

```bash
# Установите зависимости
go mod tidy

# Сборка обычного бинаря
go build -o systray-queue-app
````

### Запуск

Просто запустите бинарь. В трее появится пункт **Tasks**. Меню:

* **Добавить задачу** — ввод текста + (опционально) вложение (PNG/JPG/M4A/MP3). Файлы копируются в `attachments/` внутри каталога данных приложения. Очередь — `queue.json`.
* **Получить первую задачу** — модальное окно предпросмотра (текст + картинка/аудио).
* **Пропустить** — переместить первую задачу в конец очереди.
* **Завершить** — удалить первую задачу из очереди.
* **Выход** — завершить приложение.

Каталог данных:

* **Linux**: `~/.config/systray-queue-app/`
* **macOS**: `~/Library/Application Support/systray-queue-app/`
* **Windows**: `%AppData%\\systray-queue-app\\`

## macOS: упаковка в .app с LSUIElement=1

1. Подготовьте структуру бандла:

```
SystrayQueue.app/
└─ Contents/
   ├─ MacOS/
   │  └─ systray-queue-app      # ваш бинарь (chmod +x)
   ├─ Info.plist
   └─ Resources/
      └─ app.icns (опционально)
```

2. Используйте `macos/Info.plist` из репозитория. Важно поле `<key>LSUIElement</key><true/>` — оно скрывает иконку из Dock.

3. Подпись (опционально) и запуск:

```bash
chmod +x SystrayQueue.app/Contents/MacOS/systray-queue-app
open SystrayQueue.app
```

## Иконки

* Для macOS можно задать монохромную template-иконку: `systray.SetTemplateIcon(templatePNG, templatePNG)`.
* Для Windows/Linux — `systray.SetIcon(iconBytes)` (лучше ICO для Windows, PNG на Linux).

В текущем коде строки закомментированы — приложение работает и без явной иконки (будет заголовок/tooltip). Чтобы добавить, вставьте свои байты иконок и раскомментируйте.

## Заметки по зависимостям

* `github.com/getlantern/systray` — системный трей.
* `github.com/ncruces/zenity` — нативные системные диалоги ввода текста и выбора файла.
* `github.com/webview/webview_go` — компактное окно предпросмотра (HTML с `<img>`/`<audio>`). На Linux потребует WebKitGTK.

````

---

## macos/Info.plist

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple Computer//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>SystrayQueue</string>
	<key>CFBundleDisplayName</key>
	<string>SystrayQueue</string>
	<key>CFBundleIdentifier</key>
	<string>com.example.systray-queue</string>
	<key>CFBundleVersion</key>
	<string>1.0</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
	<key>CFBundleExecutable</key>
	<string>systray-queue-app</string>
	<key>LSMinimumSystemVersion</key>
	<string>10.13</string>
	<!-- Скрыть иконку из Dock -->
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
````

---

## Примечания по кроссплатформенности

* **Файловые диалоги**: zenity использует нативные API на всех ОС.
* **Предпросмотр**: мини-окно webview (HTML) — без тяжёлых фреймворков.
* **Хранилище**: JSON на диске + папка `attachments/` с копиями файлов.
* **Потокобезопасность**: операции над очередью под мьютексом.
* **Graceful exit**: `systray.Quit()` и `onExit()` для возможной финализации.

```}
```
