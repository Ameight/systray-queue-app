# systray-queue-app

## Сборка

```bash
# Установите зависимости
go mod tidy

# Сборка обычного бинаря
go build -o systray-queue-app


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


# Требует

```
brew install pngpaste
```