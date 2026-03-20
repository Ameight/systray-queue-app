# systray-queue-app

Приложение-очередь задач в системном трее для macOS. Задачи хранятся локально, поддерживают Markdown и вложения (изображения, аудио). Управление — через меню трея, горячие клавиши и браузерный интерфейс.

## Требования

- Go 1.21+
- macOS (Apple Silicon)
- Xcode Command Line Tools: `xcode-select --install`

## Сборка и запуск

```bash
# Клонировать репозиторий
git clone https://github.com/Ameight/systray-queue-app
cd systray-queue-app

# Скачать зависимости
go mod tidy

# Собрать бинарь
make build

# Запустить
./systray-queue-app
```

Для разработки можно запускать напрямую без сборки:

```bash
make run
```

## Упаковка в .app (рекомендуется)

Без `.app`-бандла macOS не скроет иконку из Dock и могут возникнуть проблемы с правами доступа к UI.

```bash
# Создать структуру бандла
mkdir -p SystrayQueue.app/Contents/MacOS
mkdir -p SystrayQueue.app/Contents/Resources

# Скопировать бинарь и plist
cp systray-queue-app SystrayQueue.app/Contents/MacOS/
cp macos/Info.plist SystrayQueue.app/Contents/

# Сделать исполняемым и запустить
chmod +x SystrayQueue.app/Contents/MacOS/systray-queue-app
open SystrayQueue.app
```

## Автозапуск при входе

В меню трея: **Settings → Launch at login**.

Флаг сохраняется в `~/Library/LaunchAgents/com.example.systray-queue.plist`. Работает только если приложение запущено как `.app`-бандл или по полному пути к бинарю.

## Меню трея

| Пункт | Действие |
|---|---|
| **Add task…** | Быстрое добавление задачи через диалог |
| **Add task (advanced)…** | Расширенный редактор в браузере (Markdown + вложение) |
| **View current task…** | Просмотр первой задачи в браузере |
| **Manage order…** | Список всех задач, перетаскивание для смены порядка |
| **Skip** | Переместить текущую задачу в конец очереди |
| **Done** | Завершить и удалить текущую задачу |
| **Quit** | Выйти из приложения |

## Горячие клавиши

По умолчанию (настраиваются в `key-config.yaml`):

| Комбинация | Действие |
|---|---|
| `Ctrl+Alt+Q` | Открыть текущую задачу |
| `Ctrl+Alt+A` | Расширенный редактор добавления |
| `Ctrl+Alt+S` | Пропустить текущую задачу |
| `Ctrl+Alt+D` | Завершить текущую задачу |
| `Ctrl+Alt+M` | Открыть управление очередью |

## Настройка горячих клавиш

Файл `key-config.yaml` создаётся автоматически при первом запуске в каталоге данных приложения (`~/Library/Application Support/systray-queue-app/key-config.yaml`).

```yaml
version: 1
hotkeys:
  show_first:
    enabled: true
    combo: "ctrl+alt+q"
  add_from_clipboard:
    enabled: true
    combo: "ctrl+alt+a"
  skip:
    enabled: true
    combo: "ctrl+alt+s"
  complete:
    enabled: true
    combo: "ctrl+alt+d"
  manage_queue:
    enabled: true
    combo: "ctrl+alt+m"
```

Поддерживаемые модификаторы:

| Имя в конфиге | Клавиша macOS |
|---|---|
| `ctrl` | ⌃ Control |
| `alt` или `option` | ⌥ Option |
| `shift` | ⇧ Shift |
| `cmd` | ⌘ Command |

Поддерживаемые клавиши: `a`–`z`, `0`–`9`, `f1`–`f12`, `space`, `enter`, `tab`, `esc`.

Чтобы отключить отдельный хоткей: `enabled: false`.

## Добавление задачи

**Быстрое добавление** (меню → *Add task…*): диалоговое окно с текстом. Поддерживает Markdown.

**Расширенный редактор** (меню → *Add task (advanced)…*): открывается в браузере, позволяет написать Markdown и прикрепить файл (изображение или аудио).

Поддерживаемые форматы вложений: `.png`, `.jpg`, `.jpeg`, `.webp`, `.gif`, `.mp3`, `.m4a`, `.wav`, `.ogg`.

## Данные приложения

Все данные хранятся в `~/Library/Application Support/systray-queue-app/`:

```
systray-queue-app/
├── queue.json          # очередь задач
├── attachments/        # вложения к задачам
└── key-config.yaml     # настройки горячих клавиш
```

Очередь можно редактировать вручную — это обычный JSON-файл.
