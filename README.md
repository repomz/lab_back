# Lab Back

API агрегатора лабораторных результатов. Реализованы JWT-аутентификация, роли пациента и врача, загрузка PDF/изображений, двухпроходный локальный OCR с оценкой уверенности, динамическая схема показателей в MongoDB, опциональная нормализация через DeepSeek, история, выдача доступа и консультации.

## Запуск

```bash
cp .env.example .env
docker compose -f ../lab_deploy/compose.yaml up --build
```

API: `http://localhost:8080`, проверка: `GET /health`.

DeepSeek используется только когда задан `DEEPSEEK_API_KEY`. Без ключа остаются OCR, базовый парсер референсов и осторожная rule-based сводка. ИИ-резюме не является диагнозом; интерфейс всегда показывает этот дисклеймер.

Все обращения к DeepSeek проходят через общий серверный предохранитель: по умолчанию не более 30 запросов в минуту, 300 в час и двух одновременных запросов с timeout 45 секунд. Пользовательские AI-endpoints дополнительно ограничены 6 запросами в минуту и 60 в час на авторизованного пользователя. Значения настраиваются через `DEEPSEEK_REQUESTS_PER_MINUTE`, `DEEPSEEK_REQUESTS_PER_HOUR`, `DEEPSEEK_MAX_CONCURRENT`, `DEEPSEEK_TIMEOUT_SECONDS`, `AI_USER_REQUESTS_PER_MINUTE` и `AI_USER_REQUESTS_PER_HOUR`.

OCR выполняется через durable MongoDB queue. Upload и reprocess возвращают `202 Accepted`, а worker атомарно захватывает анализ с lease, публикует этапы обработки и повторяет временные ошибки с backoff. По умолчанию работает один worker, три попытки, timeout 180 секунд и lease 240 секунд; параметры задаются через `OCR_WORKER_COUNT`, `OCR_JOB_MAX_ATTEMPTS`, `OCR_JOB_TIMEOUT_SECONDS` и `OCR_JOB_LEASE_SECONDS`. При увеличении CPU число workers можно повысить без изменения API.

## Основные маршруты

- `POST /api/v1/auth/register`, `POST /api/v1/auth/login`
- `GET /api/v1/me`, `GET /api/v1/doctors`
- `POST /api/v1/analyses` (`multipart/form-data`), `GET /api/v1/analyses`
- `GET /api/v1/analyses/{id}`, `GET /api/v1/analyses/{id}/file`
- `GET /api/v1/analyses/{id}/report.pdf` — итоговый PDF с распознанными показателями
- `DELETE /api/v1/analyses/{id}` — удаление записи и сохранённого оригинала владельцем
- `POST /api/v1/analyses/{id}/reprocess` — повторное распознавание сохранённого оригинала
- `POST /api/v1/analyses/{id}/share`
- `GET/POST/PATCH /api/v1/consultations`

Для production нужны TLS, секрет из secret manager, антивирусная проверка файлов, очередь фоновой обработки, верификация врачей, аудит обращений, отзыв доступа и согласия в соответствии с применимым законодательством.
