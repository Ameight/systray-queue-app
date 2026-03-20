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

	"github.com/Ameight/systray-queue-app/internal/hotkeys"
	"github.com/Ameight/systray-queue-app/internal/queue"
	"github.com/Ameight/systray-queue-app/internal/ui"
	"github.com/Ameight/systray-queue-app/internal/util"
)

type Server struct {
	q       *queue.TaskQueue
	baseDir string

	once sync.Once
	url  string
	err  error

	reloadHotkeys func() error
}

func New(q *queue.TaskQueue, baseDir string) *Server {
	return &Server{q: q, baseDir: baseDir}
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

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	go preloadWhisperModel()

	return "http://" + ln.Addr().String() + "/", nil
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
	page := ui.RenderPage("Add task", renderAddHTML())
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
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

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

	// For image/audio attachments uploaded via the browser, serve via /attachment endpoint.
	taskText := t.Text
	if t.AttachmentType == queue.AttachmentImage && t.AttachmentPath != "" {
		name := filepath.Base(t.AttachmentPath)
		imgURL := "/attachment?name=" + url.QueryEscape(name)
		taskText += "\n\n![attachment](" + imgURL + ")\n"
	}

	frag, err := ui.RenderTaskHTML(queue.Task{
		ID:             t.ID,
		Text:           taskText,
		CreatedAt:      t.CreatedAt,
		AttachmentPath: t.AttachmentPath,
		AttachmentType: t.AttachmentType,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// For audio, override the file:// URL with a server-relative one.
	if t.AttachmentType == queue.AttachmentAudio && t.AttachmentPath != "" {
		name := filepath.Base(t.AttachmentPath)
		audioTag := fmt.Sprintf(`<audio controls src="/attachment?name=%s"></audio>`, url.QueryEscape(name))
		frag += "\n" + audioTag
	}

	body := fmt.Sprintf(`<h1>Current task</h1>
<div class="row">
  <button onclick="doAction('done')">Done</button>
  <button onclick="doAction('skip')">Skip</button>
  <button onclick="location.href='/add'">Add</button>
  <button onclick="location.href='/'">Manage order</button>
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

func renderAddHTML() string {
	return `<h1>Add task</h1>
<form id="task-form" action="/add_submit" method="post" enctype="multipart/form-data">
  <div class="row">
    <button type="submit">Save</button>
    <button type="button" onclick="location.href='/view'">Cancel</button>
  </div>
  <p class="muted">Markdown supported. You can attach an image or record a voice note (requires <a href="https://github.com/openai/whisper" target="_blank">whisper</a>).</p>
  <p><textarea name="text" id="task-text" placeholder="Write task in Markdown..."></textarea></p>
  <p><label>Attachment: <input type="file" name="attachment" accept="image/*,audio/*" /></label></p>

  <div style="margin-top:12px">
    <div class="row">
      <button type="button" id="rec-btn">Record voice note</button>
      <span id="rec-status" class="muted"></span>
    </div>
    <div id="rec-player" style="display:none;margin-top:8px"></div>
  </div>

  <input type="hidden" name="voice_attachment" id="voice-attachment-name" value="">
</form>

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
	b.WriteString(`<style>
        body{font-family:system-ui,-apple-system,Segoe UI,Roboto,Ubuntu,Cantarell,Noto Sans,sans-serif;margin:20px;max-width:900px}
        h1{font-size:20px;margin:0 0 12px}
        .row{display:flex;gap:10px;align-items:center;margin:12px 0;flex-wrap:wrap}
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
	b.WriteString(`<div class="row"><button id="save">Save order</button><button onclick="location.href='/add'">Add</button><button onclick="location.href='/view'">View</button><button onclick="location.href='/settings'">Settings</button><span id="status"></span></div>`)
	b.WriteString(`<ul id="list">`)
	for i, t := range tasks {
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
func preloadWhisperModel() {
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

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var cfg hotkeys.KeyConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
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
  const body = JSON.stringify({ version: 1, hotkeys });
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

func OpenBrowser(url string) error {
	return util.OpenBrowser(url)
}
