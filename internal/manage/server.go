package manage

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ameight/systray-queue-app/internal/queue"
	"github.com/Ameight/systray-queue-app/internal/util"
)

type Server struct {
	q       *queue.TaskQueue
	baseDir string

	once sync.Once
	url  string
	err  error
}

func New(q *queue.TaskQueue, baseDir string) *Server {
	return &Server{q: q, baseDir: baseDir}
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

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()

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

	// redirect to view
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

	// store under attachments with unique name
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

	frag, err := ui.RenderTaskHTML(t)
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

func renderAddHTML() string {
	return `<h1>Add task</h1>
<form action="/add_submit" method="post" enctype="multipart/form-data">
  <div class="row"><button type="submit">Save</button><button type="button" onclick="location.href='/view'">Cancel</button></div>
  <div class="muted">Markdown supported. You can attach an image or audio file.</div>
  <p><textarea name="text" placeholder="Write task in Markdown..."></textarea></p>
  <p><input type="file" name="attachment" /></p>
</form>`
}

func renderManageHTML(tasks []queue.Task) string {
	// reuse DnD UI similar to previous version + links
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
	b.WriteString(`<div class="row"><button id="save">Save order</button><button onclick="location.href='/add'">Add</button><button onclick="location.href='/view'">View</button><span id="status"></span></div>`)
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

func OpenBrowser(url string) error {
	return util.OpenBrowser(url)
}
