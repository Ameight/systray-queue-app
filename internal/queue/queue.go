package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

type TaskQueue struct {
	mu             sync.Mutex
	Tasks          []Task `json:"tasks"`
	filePath       string
	attachmentsDir string
}

func NewTaskQueue(baseDir string) (*TaskQueue, error) {
	q := &TaskQueue{
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

func (q *TaskQueue) loadLocked() error {
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

func (q *TaskQueue) saveLocked() error {
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(q.filePath, data, 0644)
}

func (q *TaskQueue) enqueue(t Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Tasks = append(q.Tasks, t)
	return q.saveLocked()
}

func (q *TaskQueue) getAll() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	res := make([]Task, len(q.Tasks))
	copy(res, q.Tasks)
	return res
}

func (q *TaskQueue) peek() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 {
		return Task{}, false
	}
	return q.Tasks[0], true
}

func (q *TaskQueue) skip() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) <= 1 {
		return nil
	}
	first := q.Tasks[0]
	q.Tasks = append(q.Tasks[1:], first)
	return q.saveLocked()
}

func (q *TaskQueue) complete() (Task, error) {
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

func (q *TaskQueue) reorderByIndicesLocked(order []int) error {
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

func (q *TaskQueue) reorderByIndices(order []int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reorderByIndicesLocked(order)
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
