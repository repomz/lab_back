# Saved patient health summary

`GET /api/v1/me/health-summary` first reads `health_summaries` in MongoDB.
Successful text and its generation time are persisted by patient ID plus the
SHA-256 fingerprint of completed studies. No TTL: unchanged results stay readable
after another login, backend restart or redeployment. Historical responses from
before this feature were not stored and cannot be recovered by the cache.

Fingerprint includes study IDs, clinical content, printed references, dates and
reviews. It excludes sharing metadata, array order and `updated_at`. Adding,
deleting or correcting a completed study selects another snapshot. Pending or
failed OCR does not replace the available clinical data. Profile-only edits do
not regenerate the response: this cache follows the user's study-set contract.

Cache hits do not consume the AI request quota. On a miss, concurrent calls in the
current single backend process coalesce, including calls from multiple devices.
Generation continues for up to 90 seconds if its initial HTTP caller disconnects.
Errors, empty provider responses and fallback text are not cached as successful AI
summaries. Database read failure never triggers an uncached provider request.

The frontend keeps the current response/promise while its home screen is mounted,
shows the saved timestamp and discards responses belonging to a changed study set.
No medical summary is written into browser localStorage. Account erasure removes
the persistent summary records. If multiple API replicas are introduced, replace
the in-process singleflight with a distributed generation lease to avoid duplicate
provider requests on simultaneous cache misses across replicas.
