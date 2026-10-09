# Включи мозг — навык Алисы

Go-сервер для Яндекс Диалогов: `POST /api`, короткие ответы OpenAI, история разговора в Redis. Для Synology DS425+ используется готовый образ `dmtrigub/alice-gpt:ds425plus` (`linux/amd64`). Собирать его на NAS не нужно.

## Запуск в Synology Container Manager

1. Создайте на NAS папку `/volume1/docker/alice-gpt` и положите в неё только [docker-compose.yml](docker-compose.yml) и `.env`. Если `.env` ещё нет, скопируйте [.env.example](.env.example) под именем `.env`. В Finder скрытые файлы показывает `⌘⇧.`.
2. Заполните в `.env`:
   - `GPT_API_KEY` — ключ из [OpenAI API Platform](https://platform.openai.com/api-keys). Баланс ChatGPT не оплачивает API; проверяйте [API Billing](https://platform.openai.com/settings/organization/billing/).
   - `ALICE_SKILL_ID` — часть адреса страницы навыка после `/developer/skills/`.
   - `WEBHOOK_TOKEN` — случайный секрет длиной от 32 символов. Создать: `openssl rand -hex 32`.
3. В **Container Manager → Проект → Создать** укажите папку и `docker-compose.yml`, затем запустите проект. Должны работать контейнеры `app` и `redis`.
4. Сервер доступен на NAS по `http://<IP-NAS>:48132`. В KeenDNS создайте доступ к веб-приложению: NAS, внутренний протокол **HTTP**, порт **48132**. Снаружи KeenDNS даёт HTTPS; порт `48132` в публичном адресе не пишите.
5. В Яндекс Диалогах укажите Webhook URL `https://<ваш-домен-KeenDNS>/api?token=<WEBHOOK_TOKEN>`. На вкладке **Тестирование** проверьте вопрос и уточнение. Для приватного навыка выберите **Тип доступа → Приватный** и нажмите **Опубликовать**.

Иконка навыка: [assets/vklyuchi-mozg-icon.png](assets/vklyuchi-mozg-icon.png).

## Проверка и обновление

```sh
curl -i http://<IP-NAS>:48132/readyz
```

`200` означает, что сервер и Redis доступны. Это не проверка ключа OpenAI. Ошибки ответа ищите в **Container Manager → Контейнер → alice-gpt-app-1 → Журнал**. `429 credit_balance_exhausted` означает отсутствие API-кредитов у организации, к которой относится ключ. Если `GPT_API_KEY` или `ALICE_SKILL_ID` пусты либо `WEBHOOK_TOKEN` короче 32 символов, приложение перезапускается; причина будет в журнале.

После изменения `.env` пересоздайте контейнер приложения через проект Container Manager или на NAS:

```sh
cd /volume1/docker/alice-gpt
sudo docker compose up -d --no-build --no-deps --force-recreate app
```

Для обновления образа сначала выполните `sudo docker compose pull app`, затем команду выше. Данные Redis находятся в Docker volume и при пересоздании `app` сохраняются. Не удаляйте проект вместе с его volume, если нужна история разговоров.

`GET /healthz` проверяет процесс, `GET /readyz` — соединение с Redis. Приложение завершает текущие запросы при остановке. Исходный код и [Dockerfile](Dockerfile) находятся здесь для изменений; готовый образ загружается из Docker Hub.
