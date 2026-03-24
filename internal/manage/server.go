package manage

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ameight/systray-queue-app/internal/autostart"
	"github.com/Ameight/systray-queue-app/internal/hotkeys"
	"github.com/Ameight/systray-queue-app/internal/queue"
	"github.com/Ameight/systray-queue-app/internal/ui"
	"github.com/Ameight/systray-queue-app/internal/util"
)

type Server struct {
	q       *queue.TaskQueue
	baseDir string
	favicon []byte

	once sync.Once
	url  string
	err  error

	reloadHotkeys func() error
}

func New(q *queue.TaskQueue, baseDir string, favicon []byte) *Server {
	return &Server{q: q, baseDir: baseDir, favicon: favicon}
}

// SetReloadFn sets a callback that is invoked after hotkey config is saved.
// The function should return an error if re-registration fails.
func (s *Server) SetReloadFn(fn func() error) {
	s.reloadHotkeys = fn
}

func (s *Server) URL() (string, error) {
	s.once.Do(func() {
		s.url, s.err = s.start()
	})
	return s.url, s.err
}

func (s *Server) start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/favicon.png", s.handleFavicon)
	mux.HandleFunc("/", s.handleManage)
	mux.HandleFunc("/reorder", s.handleReorder)
	mux.HandleFunc("/add", s.handleAdd)
	mux.HandleFunc("/add_submit", s.handleAddSubmit)
	mux.HandleFunc("/view", s.handleView)
	mux.HandleFunc("/action", s.handleAction)
	mux.HandleFunc("/attachment", s.handleAttachment)
	mux.HandleFunc("/settings", s.handleSettings)
	mux.HandleFunc("/settings/save", s.handleSettingsSave)
	mux.HandleFunc("/transcribe", s.handleTranscribe)
	mux.HandleFunc("/task_preview", s.handleTaskPreview)
	mux.HandleFunc("/task_raw", s.handleTaskRaw)
	mux.HandleFunc("/task_update", s.handleTaskUpdate)
	mux.HandleFunc("/history", s.handleHistory)
	mux.HandleFunc("/history/delete", s.handleHistoryDelete)
	mux.HandleFunc("/history/clear", s.handleHistoryClear)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	cfg, _, _ := hotkeys.LoadOrCreate(s.baseDir)
	go preloadWhisperModel(cfg.IsWhisperEnabled())

	return "http://" + ln.Addr().String() + "/", nil
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if len(s.favicon) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(s.favicon)
}

func (s *Server) handleManage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tasks := s.q.GetAll()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, renderManageHTML(tasks))
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
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
	ord := append([]int(nil), req.Order...)
	sort.Ints(ord)
	for i := range ord {
		if ord[i] != i {
			http.Error(w, "bad permutation", http.StatusBadRequest)
			return
		}
	}
	if err := s.q.ReorderByIndices(req.Order); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	io.WriteString(w, `{"ok":true}`)
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, _, _ := hotkeys.LoadOrCreate(s.baseDir)
	page := ui.RenderPage("Add task", renderAddHTML(cfg.IsWhisperEnabled()))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, page)
}

func (s *Server) handleAddSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))

	var attachmentPath string
	var attachmentType queue.AttachmentType = queue.AttachmentNone

	file, hdr, err := r.FormFile("attachment")
	if err == nil {
		defer file.Close()
		attachmentPath, attachmentType, err = s.saveUploadedAttachment(file, hdr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// If no manual attachment but a voice recording was transcribed, use it.
	if attachmentPath == "" {
		voiceFn := strings.TrimSpace(r.FormValue("voice_attachment"))
		if voiceFn != "" && !strings.Contains(voiceFn, "/") && !strings.Contains(voiceFn, "\\") && !strings.Contains(voiceFn, "..") {
			candidate := filepath.Join(s.q.AttachmentsDir(), voiceFn)
			inside, err := util.IsPathInsideDir(candidate, s.q.AttachmentsDir())
			if err == nil && inside {
				if _, err := os.Stat(candidate); err == nil {
					attachmentPath = candidate
					attachmentType = queue.AttachmentAudio
				}
			}
		}
	}

	// Text is required only when there is no attachment.
	if text == "" && attachmentPath == "" {
		http.Error(w, "text or attachment required", http.StatusBadRequest)
		return
	}

	t := queue.Task{
		ID:             strconv.FormatInt(time.Now().UnixNano(), 10),
		Text:           text,
		CreatedAt:      time.Now(),
		AttachmentPath: attachmentPath,
		AttachmentType: attachmentType,
	}
	if err := s.q.Enqueue(t); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/view", http.StatusSeeOther)
}

func (s *Server) saveUploadedAttachment(file multipart.File, hdr *multipart.FileHeader) (string, queue.AttachmentType, error) {
	name := hdr.Filename
	ext := strings.ToLower(filepath.Ext(name))
	var t queue.AttachmentType
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		t = queue.AttachmentImage
	case ".m4a", ".mp3", ".wav", ".ogg":
		t = queue.AttachmentAudio
	default:
		return "", queue.AttachmentNone, fmt.Errorf("unsupported attachment type: %s", ext)
	}

	fn := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	path := filepath.Join(s.q.AttachmentsDir(), fn)
	out, err := os.Create(path)
	if err != nil {
		return "", queue.AttachmentNone, err
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		return "", queue.AttachmentNone, err
	}
	_ = out.Sync()
	return path, t, nil
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t, ok := s.q.Peek()
	if !ok {
		page := ui.RenderPage("Queue", `<h1>Queue</h1><p class="muted">Queue is empty.</p><div class="row"><button onclick="location.href='/add'">Add task</button><button onclick="location.href='/'">Manage order</button></div>`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
		return
	}

	// Embed image/audio via /attachment endpoint so the browser can load them.
	taskText := t.Text
	if t.AttachmentPath != "" {
		name := filepath.Base(t.AttachmentPath)
		switch t.AttachmentType {
		case queue.AttachmentImage:
			taskText += "\n\n![attachment](/attachment?name=" + url.QueryEscape(name) + ")\n"
		case queue.AttachmentAudio:
			taskText += "\n\n<audio controls src=\"/attachment?name=" + url.QueryEscape(name) + "\"></audio>\n"
		}
	}

	// Pass AttachmentNone so RenderTaskHTML does not add its own file:// audio tag.
	frag, err := ui.RenderTaskHTML(queue.Task{
		ID:        t.ID,
		Text:      taskText,
		CreatedAt: t.CreatedAt,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	body := fmt.Sprintf(`<h1>Current task</h1>
<div class="row">
  <button onclick="doAction('done')">Done</button>
  <button onclick="doAction('skip')">Skip</button>
  <button onclick="location.href='/add'">Add</button>
  <button onclick="location.href='/'">Manage order</button>
  <button onclick="location.href='/history'">History</button>
</div>
<div class="card">%s</div>
<script>
async function doAction(a){
  const res = await fetch('/action', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({action:a})});
  if(!res.ok){ alert(await res.text()); return; }
  location.reload();
}
</script>`, frag)

	page := ui.RenderPage("Current task", body)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, page)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "skip":
		if err := s.q.Skip(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "done":
		if _, err := s.q.Complete(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	io.WriteString(w, `{"ok":true}`)
}

// handleAttachment serves files from the attachments directory.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.q.AttachmentsDir(), name)
	inside, err := util.IsPathInsideDir(path, s.q.AttachmentsDir())
	if err != nil || !inside {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, path)
}

func renderAddHTML(whisperEnabled bool) string {
	hint := `Markdown supported. You can attach an image or audio file.`
	if whisperEnabled {
		hint = `Markdown supported. You can attach an image or record a voice note (requires <a href="https://github.com/openai/whisper" target="_blank">whisper</a>).`
	}

	recSection := ""
	if whisperEnabled {
		recSection = `
  <div style="margin-top:12px">
    <div class="row">
      <button type="button" id="rec-btn">Record voice note</button>
      <span id="rec-status" class="muted"></span>
    </div>
    <div id="rec-player" style="display:none;margin-top:8px"></div>
  </div>

<script>
(function(){
  const btn = document.getElementById('rec-btn');
  const status = document.getElementById('rec-status');
  const player = document.getElementById('rec-player');
  const voiceField = document.getElementById('voice-attachment-name');
  const textarea = document.getElementById('task-text');

  let mediaRecorder = null;
  let chunks = [];

  btn.addEventListener('click', async () => {
    if (mediaRecorder && mediaRecorder.state === 'recording') {
      mediaRecorder.stop();
      return;
    }

    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch (e) {
      status.textContent = 'Microphone access denied: ' + e.message;
      return;
    }

    const mimeType = ['audio/webm', 'audio/ogg', 'audio/mp4'].find(m => MediaRecorder.isTypeSupported(m)) || '';
    mediaRecorder = new MediaRecorder(stream, mimeType ? { mimeType } : {});
    chunks = [];

    mediaRecorder.ondataavailable = e => { if (e.data.size > 0) chunks.push(e.data); };

    mediaRecorder.onstop = async () => {
      stream.getTracks().forEach(t => t.stop());
      btn.textContent = 'Record voice note';
      status.textContent = 'Transcribing…';

      const blob = new Blob(chunks, { type: mediaRecorder.mimeType || 'audio/webm' });

      // Show local preview while waiting for transcription.
      const url = URL.createObjectURL(blob);
      player.innerHTML = '<audio controls src="' + url + '"></audio>';
      player.style.display = '';

      try {
        const res = await fetch('/transcribe', {
          method: 'POST',
          headers: { 'Content-Type': blob.type || 'audio/webm' },
          body: blob,
        });
        const data = await res.json();

        if (data.filename) {
          voiceField.value = data.filename;
          // Replace local blob URL with server URL for the saved file.
          player.innerHTML = '<audio controls src="/attachment?name=' + encodeURIComponent(data.filename) + '"></audio>';
        }

        if (data.error) {
          status.textContent = 'Transcription error: ' + data.error;
        } else if (data.text) {
          const prefix = '\n\n---\n\u{1F3A4} **Голосовая заметка:**\n';
          textarea.value += prefix + data.text;
          status.textContent = 'Transcribed.';
        } else {
          status.textContent = 'Recording saved (no transcription).';
        }
      } catch (e) {
        status.textContent = 'Upload error: ' + e.message;
      }
    };

    mediaRecorder.start();
    btn.textContent = 'Stop recording';
    status.textContent = 'Recording…';
  });
})();
</script>`
	}

	return `<h1>Add task</h1>
<form id="task-form" action="/add_submit" method="post" enctype="multipart/form-data">
  <div class="row">
    <button type="submit">Save</button>
    <button type="button" onclick="location.href='/view'">Cancel</button>
  </div>
  <p class="muted">` + hint + `</p>
  <p><textarea name="text" id="task-text" placeholder="Write task in Markdown..."></textarea></p>
  <p><label>Attachment: <input type="file" name="attachment" accept="image/*,audio/*" /></label></p>
  <input type="hidden" name="voice_attachment" id="voice-attachment-name" value="">` + recSection + `
</form>`
}

func renderManageHTML(tasks []queue.Task) string {
	esc := func(s string) string {
		replacer := strings.NewReplacer(
			"&", "&amp;",
			"<", "&lt;",
			">", "&gt;",
			`"`, "&quot;",
		)
		return replacer.Replace(s)
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>Manage queue</title>`)
	b.WriteString(`<link rel="icon" type="image/png" href="/favicon.png">`)
	b.WriteString(`<style>
        *{box-sizing:border-box}
        body{font-family:system-ui,-apple-system,Segoe UI,Roboto,Ubuntu,Cantarell,Noto Sans,sans-serif;margin:0;padding:16px;height:100vh;display:flex;flex-direction:column;overflow:hidden}
        h1{font-size:20px;margin:0 0 10px}
        .row{display:flex;gap:10px;align-items:center;margin:0 0 10px;flex-wrap:wrap}
        button{padding:8px 12px;border-radius:8px;border:1px solid #ccc;background:#fff;cursor:pointer;font-size:14px}
        button:hover{background:#f5f5f5}
        button.primary{background:#1a73e8;color:#fff;border-color:#1a73e8}
        button.primary:hover{background:#1558b0}
        #status{font-size:12px;color:#666}
        .main{display:flex;gap:16px;flex:1;min-height:0}
        .left-panel{width:320px;flex-shrink:0;display:flex;flex-direction:column;min-height:0}
        .right-panel{flex:1;border:1px solid #ddd;border-radius:12px;overflow:auto;padding:16px;background:#fafafa;min-width:0}
        ul{list-style:none;padding:0;margin:0;border:1px solid #ddd;border-radius:12px;overflow-y:auto;flex:1}
        li{padding:10px 12px;border-bottom:1px solid #eee;cursor:grab;background:#fff;user-select:none;font-size:14px}
        li:last-child{border-bottom:none}
        li:hover{background:#f8f8f8}
        li.selected{background:#e8f0fe;border-left:3px solid #1a73e8;padding-left:9px}
        li.dragging{opacity:.5}
        li.over{outline:2px dashed #999;outline-offset:-2px}
        .hint{font-size:12px;color:#888;margin-top:8px}
        .muted{color:#888;font-size:14px}
        .empty-hint{color:#bbb;font-size:15px;display:flex;align-items:center;justify-content:center;height:100%;text-align:center}
        .preview-bar{display:flex;gap:8px;align-items:center;margin-bottom:12px;flex-wrap:wrap}
        textarea.edit-area{width:100%;min-height:260px;font-family:inherit;font-size:14px;padding:10px;border:1px solid #ccc;border-radius:8px;resize:vertical}
        img{max-width:100%;height:auto;border-radius:8px;border:1px solid #ddd}
        pre,code{background:#f6f8fa;border-radius:4px;padding:2px 4px}
        pre code{padding:0}
        pre{padding:12px}
        audio{width:100%;margin:8px 0}
    </style></head><body>`)
	b.WriteString(`<h1>Manage queue</h1>`)
	b.WriteString(`<div class="row"><button id="save">Save order</button><button onclick="location.href='/add'">Add</button><button onclick="location.href='/view'">View</button><button onclick="location.href='/history'">History</button><button onclick="location.href='/settings'">Settings</button><span id="status"></span></div>`)
	b.WriteString(`<div class="main">`)
	b.WriteString(`<div class="left-panel">`)
	b.WriteString(`<ul id="list">`)
	for i, t := range tasks {
		prev := t.Text
		if idx := strings.IndexByte(prev, '\n'); idx >= 0 {
			prev = prev[:idx]
		}
		if len(prev) > 100 {
			prev = prev[:100] + "…"
		}
		b.WriteString(fmt.Sprintf(`<li draggable="true" data-idx="%d" data-id="%s">%d. %s</li>`, i, esc(t.ID), i+1, esc(prev)))
	}
	b.WriteString(`</ul>`)
	b.WriteString(`<div class="hint">Drag to reorder · Click to preview</div>`)
	b.WriteString(`</div>`)
	b.WriteString(`<div class="right-panel" id="preview-panel"><div class="empty-hint">← Click a task to preview it</div></div>`)
	b.WriteString(`</div>`)
	b.WriteString(`<script>
        const list = document.getElementById('list');
        const status = document.getElementById('status');
        const panel = document.getElementById('preview-panel');
        let dragging = null;
        let dragMoved = false;
        let selectedLi = null;
        let currentId = null;

        function setStatus(msg){ status.textContent = msg || ''; }

        function escHTML(s){ return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }

        // ── Drag & drop ──────────────────────────────────────────────────────
        list.addEventListener('dragstart', (e) => {
            const li = e.target.closest('li');
            if (!li) return;
            dragging = li;
            dragMoved = true;
            li.classList.add('dragging');
            e.dataTransfer.effectAllowed = 'move';
            e.dataTransfer.setData('text/plain', li.dataset.idx);
        });

        list.addEventListener('dragend', (e) => {
            const li = e.target.closest('li');
            if (!li) return;
            li.classList.remove('dragging');
            [...list.querySelectorAll('li.over')].forEach(x => x.classList.remove('over'));
            dragging = null;
            setTimeout(() => { dragMoved = false; }, 50);
        });

        list.addEventListener('dragover', (e) => {
            e.preventDefault();
            const over = e.target.closest('li');
            if (!over || !dragging || over === dragging) return;
            over.classList.add('over');
            const rect = over.getBoundingClientRect();
            const before = (e.clientY - rect.top) < rect.height / 2;
            list.insertBefore(dragging, before ? over : over.nextSibling);
        });

        list.addEventListener('dragleave', (e) => {
            const li = e.target.closest('li');
            if (li) li.classList.remove('over');
        });

        // ── Click to preview ─────────────────────────────────────────────────
        list.addEventListener('click', (e) => {
            if (dragMoved) return;
            const li = e.target.closest('li');
            if (!li) return;
            openPreview(li);
        });

        async function openPreview(li) {
            if (selectedLi) selectedLi.classList.remove('selected');
            selectedLi = li;
            li.classList.add('selected');
            currentId = li.dataset.id;
            panel.innerHTML = '<div class="muted">Loading…</div>';
            try {
                const res = await fetch('/task_preview?id=' + encodeURIComponent(currentId));
                if (!res.ok) throw new Error(await res.text());
                showPreview(await res.text());
            } catch (err) {
                panel.innerHTML = '<div class="muted">Error: ' + escHTML(err.message) + '</div>';
            }
        }

        function showPreview(html) {
            panel.innerHTML =
                '<div class="preview-bar">' +
                  '<button onclick="enterEdit()">Edit</button>' +
                '</div>' +
                '<div id="preview-content">' + html + '</div>';
        }

        async function enterEdit() {
            panel.innerHTML = '<div class="muted">Loading…</div>';
            try {
                const res = await fetch('/task_raw?id=' + encodeURIComponent(currentId));
                if (!res.ok) throw new Error(await res.text());
                const data = await res.json();
                showEdit(data.text);
            } catch (err) {
                panel.innerHTML = '<div class="muted">Error: ' + escHTML(err.message) + '</div>';
            }
        }

        function showEdit(text) {
            panel.innerHTML =
                '<div class="preview-bar">' +
                  '<button class="primary" onclick="saveEdit()">Save</button>' +
                  '<button onclick="cancelEdit()">Cancel</button>' +
                  '<span id="edit-status" class="muted" style="margin-left:6px"></span>' +
                '</div>' +
                '<textarea class="edit-area" id="edit-ta">' + escHTML(text) + '</textarea>';
            document.getElementById('edit-ta').focus();
        }

        async function saveEdit() {
            const text = document.getElementById('edit-ta').value;
            const s = document.getElementById('edit-status');
            s.textContent = 'Saving…';
            try {
                const res = await fetch('/task_update', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({id: currentId, text})
                });
                if (!res.ok) throw new Error(await res.text());
                // update list item label
                if (selectedLi) {
                    let first = text.split('\n')[0];
                    const truncated = first.length > 100;
                    if (truncated) first = first.slice(0, 100);
                    const idx = parseInt(selectedLi.dataset.idx, 10);
                    selectedLi.textContent = (idx + 1) + '. ' + first + (truncated ? '…' : '');
                    selectedLi.dataset.idx = selectedLi.dataset.idx; // keep attrs
                }
                const r2 = await fetch('/task_preview?id=' + encodeURIComponent(currentId));
                if (r2.ok) showPreview(await r2.text());
            } catch (err) {
                document.getElementById('edit-status').textContent = 'Error: ' + escHTML(err.message);
            }
        }

        async function cancelEdit() {
            try {
                const res = await fetch('/task_preview?id=' + encodeURIComponent(currentId));
                if (res.ok) { showPreview(await res.text()); return; }
            } catch(_) {}
            panel.innerHTML = '<div class="empty-hint">← Click a task to preview it</div>';
        }

        // ── Save order ───────────────────────────────────────────────────────
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
                setTimeout(() => setStatus(''), 1200);
            } catch (err) {
                setStatus('Error: ' + err.message);
            }
        });
    </script>`)
	b.WriteString(`</body></html>`)
	return b.String()
}

func (s *Server) handleTaskPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	t, ok := s.q.GetByID(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	taskText := t.Text
	if t.AttachmentPath != "" {
		name := filepath.Base(t.AttachmentPath)
		switch t.AttachmentType {
		case queue.AttachmentImage:
			taskText += "\n\n![attachment](/attachment?name=" + url.QueryEscape(name) + ")\n"
		case queue.AttachmentAudio:
			taskText += "\n\n<audio controls src=\"/attachment?name=" + url.QueryEscape(name) + "\"></audio>\n"
		}
	}
	frag, err := ui.RenderTaskHTML(queue.Task{ID: t.ID, Text: taskText, CreatedAt: t.CreatedAt})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, frag)
}

func (s *Server) handleTaskRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	t, ok := s.q.GetByID(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	data, _ := json.Marshal(map[string]string{"text": t.Text})
	w.Write(data)
}

func (s *Server) handleTaskUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if err := s.q.UpdateText(req.ID, req.Text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	io.WriteString(w, `{"ok":true}`)
}

// handleTranscribe receives a raw audio blob, saves it, transcribes with whisper,
// and returns {"text":"...","filename":"..."}.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Limit upload to 100 MB.
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)

	// Detect extension from Content-Type.
	ct := r.Header.Get("Content-Type")
	ext := ".webm"
	switch {
	case strings.Contains(ct, "ogg"):
		ext = ".ogg"
	case strings.Contains(ct, "mp4"):
		ext = ".mp4"
	case strings.Contains(ct, "wav"):
		ext = ".wav"
	}

	fn := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	audioPath := filepath.Join(s.q.AttachmentsDir(), fn)
	out, err := os.Create(audioPath)
	if err != nil {
		http.Error(w, "cannot create file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, r.Body); err != nil {
		out.Close()
		os.Remove(audioPath)
		http.Error(w, "write error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out.Close()

	text, err := transcribeAudio(audioPath)
	if err != nil {
		// Log full details, show user a short message.
		userMsg := err.Error()
		if te, ok := err.(*transcribeError); ok {
			log.Printf("[whisper] transcription failed: %s", te.Detail)
			userMsg = te.UserMsg
		} else {
			log.Printf("[whisper] transcription failed: %v", err)
		}
		// Return the audio filename so the recording is still kept.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		data, _ := json.Marshal(map[string]string{"text": "", "filename": fn, "error": userMsg})
		w.Write(data)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	data, _ := json.Marshal(map[string]string{"text": text, "filename": fn})
	w.Write(data)
}

// whisper tiny model: URL and expected local filename inside the cache dir.
const (
	whisperTinyURL      = "https://openaipublic.azureedge.net/main/whisper/models/65147644a518d12f04e32d6f3b26facc3f8dd46e5390956a9424a650c0ce22b9/tiny.pt"
	whisperTinyFilename = "tiny.pt"
)

// whisperCacheDir returns ~/.cache/whisper (the default download_root used by the whisper Python library).
func whisperCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "whisper")
}

// preloadWhisperModel downloads the whisper model via Go's HTTP client in the
// background at startup, so the first transcription does not stall waiting for
// a download. Go's http.Client bypasses system proxy settings that can interfere
// with Python's urllib.
func preloadWhisperModel(enabled bool) {
	if !enabled {
		return
	}
	if _, err := exec.LookPath("whisper"); err != nil {
		return // whisper not installed
	}

	cacheDir := whisperCacheDir()
	if cacheDir == "" {
		return
	}
	modelPath := filepath.Join(cacheDir, whisperTinyFilename)

	// Already cached — nothing to do.
	if _, err := os.Stat(modelPath); err == nil {
		return
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return
	}

	log.Printf("[whisper] downloading tiny model to %s …", modelPath)
	resp, err := http.Get(whisperTinyURL) //nolint:noctx
	if err != nil {
		log.Printf("[whisper] model download failed: %v", err)
		return
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp(cacheDir, "tiny-*.pt.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return
	}
	tmp.Close()

	if err := os.Rename(tmpName, modelPath); err != nil {
		return
	}
	log.Printf("[whisper] tiny model ready")
}

// clearProxyEnv returns env with HTTP(S)_PROXY variables removed.
func clearProxyEnv(env []string) []string {
	out := env[:0:len(env)]
	for _, e := range env {
		key := strings.ToUpper(strings.SplitN(e, "=", 2)[0])
		if key == "HTTP_PROXY" || key == "HTTPS_PROXY" || key == "ALL_PROXY" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// whisperModel is the default model used for transcription.
// "tiny" (~39 MB) downloads fast; change to "small" or "base" for better accuracy.
const whisperModel = "tiny"

// transcribeAudio runs the whisper CLI on audioPath and returns the transcribed text.
// transcribeError carries a short user-facing message and a detailed cause for logging.
type transcribeError struct {
	UserMsg string
	Detail  string
}

func (e *transcribeError) Error() string { return e.UserMsg + ": " + e.Detail }

func transcribeAudio(audioPath string) (string, error) {
	whisperBin, err := exec.LookPath("whisper")
	if err != nil {
		return "", &transcribeError{
			UserMsg: "Whisper не установлен",
			Detail:  "binary not found in PATH; install with: pip install openai-whisper",
		}
	}
	outDir := filepath.Dir(audioPath)
	cmd := exec.Command(whisperBin, audioPath, "--model", whisperModel, "--output_format", "txt", "--output_dir", outDir)
	// Clear proxy env vars so Python's urllib does not try to tunnel through a
	// system proxy (which can interfere with loading the already-cached model).
	cmd.Env = clearProxyEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		userMsg := "Ошибка транскрипции"
		outStr := string(out)
		switch {
		case strings.Contains(outStr, "URLError") || strings.Contains(outStr, "urlopen") || strings.Contains(outStr, "RemoteDisconnected"):
			userMsg = "Не удалось загрузить модель Whisper — нет доступа к сети"
		case strings.Contains(outStr, "load_model") || strings.Contains(outStr, "download"):
			userMsg = "Ошибка загрузки модели Whisper"
		case strings.Contains(outStr, "No such file"):
			userMsg = "Аудиофайл не найден"
		}
		return "", &transcribeError{UserMsg: userMsg, Detail: fmt.Sprintf("exit: %v\n%s", err, outStr)}
	}
	// Whisper writes <basename>.txt next to the audio file.
	base := strings.TrimSuffix(filepath.Base(audioPath), filepath.Ext(audioPath))
	txtPath := filepath.Join(outDir, base+".txt")
	txtBytes, err := os.ReadFile(txtPath)
	if err != nil {
		return "", &transcribeError{
			UserMsg: "Не удалось прочитать результат транскрипции",
			Detail:  err.Error(),
		}
	}
	_ = os.Remove(txtPath)
	return strings.TrimSpace(string(txtBytes)), nil
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, _, err := hotkeys.LoadOrCreate(s.baseDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := ui.RenderPage("Settings", renderSettingsHTML(cfg))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, page)
}

type settingsSaveRequest struct {
	hotkeys.KeyConfig
	AutostartEnabled *bool `json:"autostart_enabled"`
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req settingsSaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg := req.KeyConfig
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if err := hotkeys.Validate(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := hotkeys.Save(s.baseDir, cfg); err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.reloadHotkeys != nil {
		if err := s.reloadHotkeys(); err != nil {
			http.Error(w, "reload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.AutostartEnabled != nil {
		exePath, err := os.Executable()
		if err == nil {
			if *req.AutostartEnabled {
				_ = autostart.Enable(exePath)
			} else {
				_ = autostart.Disable()
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	io.WriteString(w, `{"ok":true}`)
}

// hotkeyMeta defines display order and labels for hotkey actions.
var hotkeyMeta = []struct {
	Key   string
	Label string
}{
	{hotkeys.ActionShowFirst, "View current task"},
	{hotkeys.ActionAddQuick, "Add task (quick dialog)"},
	{hotkeys.ActionAddFromClipboard, "Add task (advanced editor)"},
	{hotkeys.ActionSkip, "Skip task"},
	{hotkeys.ActionComplete, "Complete task"},
	{hotkeys.ActionManageQueue, "Manage queue"},
}

func renderSettingsHTML(cfg hotkeys.KeyConfig) string {
	esc := func(s string) string {
		return strings.NewReplacer(`"`, "&quot;", "&", "&amp;", "<", "&lt;").Replace(s)
	}

	var b strings.Builder
	b.WriteString(`<h1>Settings</h1>`)
	b.WriteString(`<h2 style="font-size:16px;margin:20px 0 10px">Hotkeys</h2>`)
	b.WriteString(`<p class="muted">
  Modifiers:
  <code>ctrl</code> ⌃ &nbsp;·&nbsp;
  <code>alt</code> / <code>option</code> ⌥ &nbsp;·&nbsp;
  <code>shift</code> ⇧ &nbsp;·&nbsp;
  <code>cmd</code> ⌘
  &nbsp;&nbsp;|&nbsp;&nbsp;
  Keys: <code>a</code>–<code>z</code>, <code>0</code>–<code>9</code>, <code>f1</code>–<code>f12</code>
</p>`)

	b.WriteString(`<table style="border-collapse:collapse;width:100%;max-width:560px">`)
	b.WriteString(`<thead><tr>`)
	b.WriteString(`<th style="text-align:left;padding:8px 12px;border-bottom:2px solid #e0e0e0;font-weight:600">Action</th>`)
	b.WriteString(`<th style="text-align:center;padding:8px 12px;border-bottom:2px solid #e0e0e0;font-weight:600">Enabled</th>`)
	b.WriteString(`<th style="text-align:left;padding:8px 12px;border-bottom:2px solid #e0e0e0;font-weight:600">Shortcut</th>`)
	b.WriteString(`</tr></thead><tbody>`)

	for _, meta := range hotkeyMeta {
		hc := cfg.Hotkeys[meta.Key]
		checked := ""
		if hc.Enabled {
			checked = " checked"
		}
		combo := hc.Combo
		b.WriteString(fmt.Sprintf(`<tr>
  <td style="padding:10px 12px;border-bottom:1px solid #f0f0f0">%s</td>
  <td style="padding:10px 12px;border-bottom:1px solid #f0f0f0;text-align:center">
    <input type="checkbox" data-key="%s" class="hk-enabled"%s style="width:16px;height:16px;cursor:pointer">
  </td>
  <td style="padding:10px 12px;border-bottom:1px solid #f0f0f0">
    <input type="text" data-key="%s" class="hk-combo" value="%s"
      style="font-family:monospace;padding:6px 10px;border:1px solid #ccc;border-radius:6px;width:180px">
  </td>
</tr>`, meta.Label, esc(meta.Key), checked, esc(meta.Key), esc(combo)))
	}

	b.WriteString(`</tbody></table>`)

	// Whisper section
	whisperChecked := ""
	if cfg.IsWhisperEnabled() {
		whisperChecked = " checked"
	}
	b.WriteString(`<h2 style="font-size:16px;margin:28px 0 10px">Voice transcription (Whisper)</h2>`)
	b.WriteString(fmt.Sprintf(`<label style="display:flex;align-items:center;gap:10px;cursor:pointer">
  <input type="checkbox" id="whisper-enabled"%s style="width:16px;height:16px;cursor:pointer">
  Enable Whisper transcription (voice recording in Add task form)
</label>`, whisperChecked))

	// Autostart section
	autostartChecked := ""
	if autostart.IsEnabled() {
		autostartChecked = " checked"
	}
	b.WriteString(`<h2 style="font-size:16px;margin:28px 0 10px">Autostart</h2>`)
	b.WriteString(fmt.Sprintf(`<label style="display:flex;align-items:center;gap:10px;cursor:pointer">
  <input type="checkbox" id="autostart-enabled"%s style="width:16px;height:16px;cursor:pointer">
  Launch automatically when the system starts
</label>`, autostartChecked))

	b.WriteString(`<div class="row" style="margin-top:20px">`)
	b.WriteString(`<button id="save-btn">Save</button>`)
	b.WriteString(`<button onclick="location.href='/'">Back</button>`)
	b.WriteString(`<span id="status" class="muted"></span>`)
	b.WriteString(`</div>`)

	b.WriteString(`<script>
document.getElementById('save-btn').addEventListener('click', async () => {
  const status = document.getElementById('status');
  const hotkeys = {};
  document.querySelectorAll('.hk-enabled').forEach(cb => {
    const key = cb.dataset.key;
    const combo = document.querySelector('.hk-combo[data-key="' + key + '"]').value.trim();
    hotkeys[key] = { enabled: cb.checked, combo };
  });
  const whisperEnabled = document.getElementById('whisper-enabled').checked;
  const autostartEnabled = document.getElementById('autostart-enabled').checked;
  const body = JSON.stringify({ version: 1, whisper_enabled: whisperEnabled, hotkeys, autostart_enabled: autostartEnabled });
  status.textContent = 'Saving…';
  try {
    const res = await fetch('/settings/save', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
    });
    if (!res.ok) throw new Error(await res.text());
    status.textContent = 'Saved';
    setTimeout(() => status.textContent = '', 2000);
  } catch (err) {
    status.textContent = 'Error: ' + err.message;
  }
});
</script>`)

	return b.String()
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries := s.q.History().GetAll()
	page := ui.RenderPage("History", renderHistoryHTML(entries))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, page)
}

func (s *Server) handleHistoryDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.q.History().DeleteByID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	io.WriteString(w, `{"ok":true}`)
}

func (s *Server) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.q.History().Clear(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	io.WriteString(w, `{"ok":true}`)
}

func renderHistoryHTML(entries []queue.Task) string {
	esc := func(s string) string {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
	}
	fmtDuration := func(d time.Duration) string {
		d = d.Round(time.Second)
		if d < time.Minute {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
		if d < time.Hour {
			return fmt.Sprintf("%dm", int(d.Minutes()))
		}
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	fmtTime := func(t time.Time) string {
		return t.Local().Format("15:04")
	}
	fmtDate := func(t time.Time) string {
		return t.Local().Format("02 Jan 2006")
	}

	// Group entries by calendar day (local time).
	type group struct {
		label   string
		entries []queue.Task
	}
	var groups []group
	dayKey := func(t time.Time) string { return t.Local().Format("2006-01-02") }
	now := time.Now()
	today := dayKey(now)
	yesterday := dayKey(now.AddDate(0, 0, -1))
	weekAgo := now.AddDate(0, 0, -6)

	groupMap := map[string]*group{}
	var groupOrder []string
	for _, e := range entries {
		ref := e.CompletedAt
		if ref.IsZero() {
			ref = e.CreatedAt
		}
		key := dayKey(ref)
		if _, ok := groupMap[key]; !ok {
			var label string
			switch key {
			case today:
				label = "Сегодня"
			case yesterday:
				label = "Вчера"
			default:
				if ref.After(weekAgo) {
					label = ref.Local().Format("Monday, 02 Jan")
				} else {
					label = fmtDate(ref)
				}
			}
			groupMap[key] = &group{label: label}
			groupOrder = append(groupOrder, key)
		}
		groupMap[key].entries = append(groupMap[key].entries, e)
	}
	for _, k := range groupOrder {
		groups = append(groups, *groupMap[k])
	}

	var b strings.Builder
	if len(entries) == 0 {
		b.WriteString(`<p class="muted">История пуста.</p>`)
		b.WriteString(`<div class="row"><button onclick="location.href='/'">Назад</button></div>`)
		return b.String()
	}

	b.WriteString(`<div class="row" style="margin-bottom:16px">`)
	b.WriteString(`<button onclick="location.href='/'">← Назад</button>`)
	b.WriteString(`<button id="clear-all" style="color:#c00;border-color:#c00">Очистить всю историю</button>`)
	b.WriteString(`</div>`)

	for _, g := range groups {
		b.WriteString(fmt.Sprintf(`<h2 style="font-size:15px;margin:20px 0 8px;color:#555">%s</h2>`, esc(g.label)))
		b.WriteString(`<div class="history-group">`)
		for _, e := range g.entries {
			// Preview: first non-empty line, truncated.
			preview := e.Text
			if idx := strings.IndexByte(preview, '\n'); idx >= 0 {
				preview = preview[:idx]
			}
			preview = strings.TrimSpace(preview)
			if len(preview) > 120 {
				preview = preview[:120] + "…"
			}

			var timeLine string
			if !e.StartedAt.IsZero() && !e.CompletedAt.IsZero() {
				dur := fmtDuration(e.CompletedAt.Sub(e.StartedAt))
				timeLine = fmt.Sprintf(`<span class="ts">Начало: %s · Конец: %s · %s</span>`,
					fmtTime(e.StartedAt), fmtTime(e.CompletedAt), dur)
			} else if !e.CompletedAt.IsZero() {
				timeLine = fmt.Sprintf(`<span class="ts">Завершено: %s</span>`, fmtTime(e.CompletedAt))
			} else {
				timeLine = fmt.Sprintf(`<span class="ts">Создано: %s</span>`, fmtTime(e.CreatedAt))
			}

			b.WriteString(fmt.Sprintf(`<div class="history-item" data-id="%s">`, esc(e.ID)))
			b.WriteString(fmt.Sprintf(`<div class="history-text">%s</div>`, esc(preview)))
			b.WriteString(fmt.Sprintf(`<div class="history-meta">%s<button class="del-btn" data-id="%s">×</button></div>`, timeLine, esc(e.ID)))
			b.WriteString(`</div>`)
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`<style>
.history-group{display:flex;flex-direction:column;gap:4px}
.history-item{background:#fff;border:1px solid #e8e8e8;border-radius:8px;padding:10px 12px;display:flex;flex-direction:column;gap:4px}
.history-text{font-size:14px;line-height:1.4}
.history-meta{display:flex;align-items:center;justify-content:space-between;gap:8px}
.ts{font-size:12px;color:#888}
.del-btn{background:none;border:none;cursor:pointer;font-size:16px;color:#bbb;padding:0 4px;line-height:1;border-radius:4px}
.del-btn:hover{color:#c00;background:#fff0f0}
</style>`)

	b.WriteString(`<script>
document.querySelectorAll('.del-btn').forEach(btn => {
  btn.addEventListener('click', async () => {
    const id = btn.dataset.id;
    const item = document.querySelector('.history-item[data-id="' + id + '"]');
    try {
      const res = await fetch('/history/delete', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({id})
      });
      if (!res.ok) throw new Error(await res.text());
      item.remove();
    } catch (e) { alert('Ошибка: ' + e.message); }
  });
});

const clearBtn = document.getElementById('clear-all');
if (clearBtn) {
  clearBtn.addEventListener('click', async () => {
    if (!confirm('Удалить всю историю?')) return;
    try {
      const res = await fetch('/history/clear', {method: 'POST'});
      if (!res.ok) throw new Error(await res.text());
      location.reload();
    } catch (e) { alert('Ошибка: ' + e.message); }
  });
}
</script>`)

	return b.String()
}

func OpenBrowser(url string) error {
	return util.OpenBrowser(url)
}
