package systray_queue_app

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
	webview "github.com/webview/webview_go"
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
	// Загрузка существующей очереди, если есть
	_ = q.load()
	return q, nil
}

func (q *taskQueue) save() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
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
	return q.save()
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
	return q.save()
}

func (q *taskQueue) complete() (Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 {
		return Task{}, errors.New("queue is empty")
	}
	first := q.Tasks[0]
	q.Tasks = q.Tasks[1:]
	if err := q.save(); err != nil {
		return Task{}, err
	}
	return first, nil
}

// ====== ПУТИ ДАННЫХ ======

func appDataDir() (string, error) {
	// ~/.local/share/appname (Linux), ~/Library/Application Support/appname (macOS), %AppData%\\appname (Windows)
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
			if ext == ".png" || ext == ".jpg" || ext == ".jpeg" {
				aType = AttachmentImage
			}
			if ext == ".m4a" || ext == ".mp3" {
				aType = AttachmentAudio
			}
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
			if c == ' ' {
				b.WriteString("%20")
			} else {
				b.WriteByte(c)
			}
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
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

// ====== ТРЕЙ ======

func onReady() {
	// Инициализация данных
	baseDir, err := appDataDir()
	if err != nil {
		log.Fatal(err)
	}
	q, err := newTaskQueue(baseDir)
	if err != nil {
		log.Fatal(err)
	}

	// Иконка (необязательно). Для macOS можно использовать монохромный template PNG.
	// systray.SetTemplateIcon(iconTemplatePNG, iconTemplatePNG) // TODO: подставьте свои байты PNG
	// systray.SetIcon(iconRegularICOorPNG)                      // Windows/Linux

	systray.SetTitle("Tasks")
	systray.SetTooltip("Очередь задач")

	mAdd := systray.AddMenuItem("Добавить задачу", "Добавить новую задачу")
	mShow := systray.AddMenuItem("Получить первую задачу", "Показать первую задачу")
	mSkip := systray.AddMenuItem("Пропустить задачу", "Переместить первую задачу в конец")
	mDone := systray.AddMenuItem("Завершить задачу", "Удалить первую задачу")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Завершить приложение")

	// Обновление динамического тултипа с количеством
	updateTooltip := func() {
		// читаем без гонок
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
	// На macOS скрываем док-иконку при запуске вне .app — это делается plist-ом в сборке .app.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx

	systray.Run(onReady, onExit)
}
