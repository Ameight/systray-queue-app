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
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"github.com/ncruces/zenity"
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

// loadLocked читает очередь из файла (если есть). Захватывает лок сам.
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
	// читаем в временную структуру, затем переносим
	var tmp struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	q.Tasks = tmp.Tasks
	return nil
}

// saveLocked пишет текущую очередь на диск. Предполагается, что лок уже удерживается.
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

// ====== ПУТИ ДАННЫХ / УТИЛИТЫ ======

func appDataDir() (string, error) {
	// Linux: ~/.config/app
	// macOS: ~/Library/Application Support/app
	// Windows: %AppData%\app
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

// makeTemplateIcon генерирует простую монохромную PNG-иконку 16x16 (подходит как template для macOS).
func makeTemplateIcon() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	// прозрачный фон
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{0, 0, 0, 0}}, image.Point{}, draw.Src)
	// чёрный кружок в центре
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

// ====== UI ДИАЛОГИ ======

func showAddTaskDialog(q *taskQueue) {
	// Ввод текста
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

	// Выбор вложения (необязательно)
	var attachPath string
	var aType AttachmentType = AttachmentNone
	if err := zenity.Question(
		"Хотите прикрепить файл? (PNG/JPG/M4A/MP3)",
		zenity.Title("Вложение"),
		zenity.OKLabel("Да"),
		zenity.CancelLabel("Нет"),
	); err == nil {
		filters := []zenity.FileFilter{
			{Name: "Изображения (PNG/JPG)", Patterns: []string{"*.png", "*.jpg", "*.jpeg"}},
			{Name: "Аудио (M4A/MP3)", Patterns: []string{"*.m4a", "*.mp3"}},
		}
		fp, ferr := zenity.SelectFile(
			zenity.Title("Выберите файл"),
			zenity.FileFilters(filters),
		)
		if ferr == nil && fp != "" {
			attachPath = fp
			ext := strings.ToLower(filepath.Ext(fp))
			if ext == ".png" || ext == ".jpg" || ext == ".jpeg" {
				aType = AttachmentImage
			} else if ext == ".m4a" || ext == ".mp3" {
				aType = AttachmentAudio
			}
		}
	}

	// Копируем вложение в каталог приложения
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

	// Сохраняем задачу
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

	msg := t.Text
	switch t.AttachmentType {
	case AttachmentImage:
		msg += "\n\nВложение: изображение (можно открыть для просмотра)"
	case AttachmentAudio:
		msg += "\n\nВложение: аудио (можно воспроизвести системным плеером)"
	}

	err := zenity.Question(
		msg,
		zenity.Title("Первая задача"),
		zenity.OKLabel("Ок"),
		zenity.ExtraButton("Открыть вложение"),
	)

	switch {
	case err == nil:
		// Нажата “Ок” — просто закрываем диалог
	case errors.Is(err, zenity.ErrExtraButton):
		// Нажато “Открыть вложение”
		if t.AttachmentPath != "" {
			_ = openWithSystem(t.AttachmentPath)
		}
	case errors.Is(err, zenity.ErrCanceled):
		// Окно закрыли крестиком — ничего не делаем
	default:
		_ = zenity.Error(fmt.Sprintf("Ошибка диалога: %v", err), zenity.Title("Ошибка"))
	}
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

	// Ставим простую монохромную template-иконку, сгенерированную на лету
	icon := makeTemplateIcon()
	systray.SetTemplateIcon(icon, icon)

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
	// Освобождение ресурсов при выходе (если понадобится)
}

func main() {
	// macOS: systray.Run должен быть вызван на главном OS-потоке
	runtime.LockOSThread()
	systray.Run(onReady, onExit)
}
