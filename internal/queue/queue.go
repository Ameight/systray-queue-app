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
	StartedAt      time.Time      `json:"started_at,omitempty"`
	CompletedAt    time.Time      `json:"completed_at,omitempty"`
	AttachmentPath string         `json:"attachment_path,omitempty"`
	AttachmentType AttachmentType `json:"attachment_type,omitempty"`
}

type TaskHistory struct {
	mu       sync.Mutex
	Entries  []Task `json:"entries"`
	filePath string
}

func NewTaskHistory(baseDir string) (*TaskHistory, error) {
	h := &TaskHistory{filePath: filepath.Join(baseDir, "history.json")}
	if err := h.load(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *TaskHistory) load() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, err := os.ReadFile(h.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			h.Entries = nil
			return nil
		}
		return err
	}
	var tmp struct {
		Entries []Task `json:"entries"`
	}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	h.Entries = tmp.Entries
	return nil
}

func (h *TaskHistory) saveLocked() error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(h.filePath, data, 0644)
}

func (h *TaskHistory) Add(t Task) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Entries = append([]Task{t}, h.Entries...)
	return h.saveLocked()
}

func (h *TaskHistory) GetAll() []Task {
	h.mu.Lock()
	defer h.mu.Unlock()
	res := make([]Task, len(h.Entries))
	copy(res, h.Entries)
	return res
}

func (h *TaskHistory) DeleteByID(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, e := range h.Entries {
		if e.ID == id {
			h.Entries = append(h.Entries[:i], h.Entries[i+1:]...)
			return h.saveLocked()
		}
	}
	return fmt.Errorf("history entry not found: %s", id)
}

func (h *TaskHistory) Clear() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Entries = nil
	return h.saveLocked()
}

type TaskQueue struct {
	mu             sync.Mutex
	Tasks          []Task `json:"tasks"`
	filePath       string
	attachmentsDir string
	history        *TaskHistory
}

func NewTaskQueue(baseDir string) (*TaskQueue, error) {
	q := &TaskQueue{
		filePath:       filepath.Join(baseDir, "queue.json"),
		attachmentsDir: filepath.Join(baseDir, "attachments"),
	}
	if err := os.MkdirAll(q.attachmentsDir, 0o755); err != nil {
		return nil, err
	}
	history, err := NewTaskHistory(baseDir)
	if err != nil {
		return nil, err
	}
	q.history = history
	if err := q.loadLocked(); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *TaskQueue) History() *TaskHistory {
	return q.history
}

func (q *TaskQueue) AttachmentsDir() string {
	return q.attachmentsDir
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
	// First task in queue is already active — set StartedAt if missing.
	if len(q.Tasks) > 0 && q.Tasks[0].StartedAt.IsZero() {
		q.Tasks[0].StartedAt = time.Now()
	}
	return nil
}

func (q *TaskQueue) saveLocked() error {
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(q.filePath, data, 0644)
}

func (q *TaskQueue) Enqueue(t Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 {
		t.StartedAt = time.Now()
	}
	q.Tasks = append(q.Tasks, t)
	return q.saveLocked()
}

func (q *TaskQueue) GetAll() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	res := make([]Task, len(q.Tasks))
	copy(res, q.Tasks)
	return res
}

func (q *TaskQueue) Peek() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) == 0 {
		return Task{}, false
	}
	return q.Tasks[0], true
}

func (q *TaskQueue) Skip() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Tasks) <= 1 {
		return nil
	}
	first := q.Tasks[0]
	q.Tasks = append(q.Tasks[1:], first)
	// New first task — mark when it became active.
	if q.Tasks[0].StartedAt.IsZero() {
		q.Tasks[0].StartedAt = time.Now()
	}
	return q.saveLocked()
}

func (q *TaskQueue) Complete() (Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.Tasks) == 0 {
		return Task{}, nil
	}

	task := q.Tasks[0]
	task.CompletedAt = time.Now()
	if task.StartedAt.IsZero() {
		task.StartedAt = task.CreatedAt
	}
	q.Tasks = q.Tasks[1:]

	// The next task becomes active — mark when it started.
	if len(q.Tasks) > 0 && q.Tasks[0].StartedAt.IsZero() {
		q.Tasks[0].StartedAt = time.Now()
	}

	if err := q.saveLocked(); err != nil {
		return Task{}, err
	}

	if q.history != nil {
		_ = q.history.Add(task)
	}

	if task.AttachmentPath != "" && q.attachmentsDir != "" {
		inside, err := isPathInsideDir(task.AttachmentPath, q.attachmentsDir)
		if err == nil && inside {
			_ = os.Remove(task.AttachmentPath)
		}
	}

	return task, nil
}

func (q *TaskQueue) GetByID(id string) (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

func (q *TaskQueue) UpdateText(id, text string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.Tasks {
		if q.Tasks[i].ID == id {
			q.Tasks[i].Text = text
			return q.saveLocked()
		}
	}
	return fmt.Errorf("task not found: %s", id)
}

func (q *TaskQueue) UpdateTask(id, text, attachmentPath string, attachmentType AttachmentType) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.Tasks {
		if q.Tasks[i].ID == id {
			q.Tasks[i].Text = text
			if attachmentPath != "" {
				q.Tasks[i].AttachmentPath = attachmentPath
				q.Tasks[i].AttachmentType = attachmentType
			}
			return q.saveLocked()
		}
	}
	return fmt.Errorf("task not found: %s", id)
}

func (q *TaskQueue) DeleteByID(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, t := range q.Tasks {
		if t.ID == id {
			q.Tasks = append(q.Tasks[:i], q.Tasks[i+1:]...)
			if i == 0 && len(q.Tasks) > 0 && q.Tasks[0].StartedAt.IsZero() {
				q.Tasks[0].StartedAt = time.Now()
			}
			return q.saveLocked()
		}
	}
	return fmt.Errorf("task not found: %s", id)
}

func (q *TaskQueue) CompleteByID(id string) (Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, t := range q.Tasks {
		if t.ID == id {
			t.CompletedAt = time.Now()
			if t.StartedAt.IsZero() {
				t.StartedAt = t.CreatedAt
			}
			q.Tasks = append(q.Tasks[:i], q.Tasks[i+1:]...)
			if i == 0 && len(q.Tasks) > 0 && q.Tasks[0].StartedAt.IsZero() {
				q.Tasks[0].StartedAt = time.Now()
			}
			if err := q.saveLocked(); err != nil {
				return Task{}, err
			}
			if q.history != nil {
				_ = q.history.Add(t)
			}
			if t.AttachmentPath != "" && q.attachmentsDir != "" {
				inside, err := isPathInsideDir(t.AttachmentPath, q.attachmentsDir)
				if err == nil && inside {
					_ = os.Remove(t.AttachmentPath)
				}
			}
			return t, nil
		}
	}
	return Task{}, fmt.Errorf("task not found: %s", id)
}

func (q *TaskQueue) ReorderByIndices(order []int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reorderByIndicesLocked(order)
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

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

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

	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

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

	if rel == "." {
		return false, nil
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.IsAbs(rel) {
		return false, nil
	}
	return true, nil
}