# Multiple studies in one source

Uploads remain synchronous: extraction, independent visual verification, per-study
interpretation and persistence complete before the response. The extraction schema
is `{"studies":[...]}`; older single-study model responses remain readable.

Separate independent reports/specimens/dates, not individual rows of one panel.
CBC, differential and ESR from one blood sample stay together; urine microalbumin
on the same printed page is a different study. Continuations across PDF pages
stay together. PDFs over 10 pages are rejected instead of silently truncated.

The HTTP response preserves the existing primary `Analysis` object and adds
`related_analyses` for the other saved studies. Each receives its own original
file copy, title, category, values and review. `source_study_index/count` identify
a separated source. Reprocessing such a record requires an unambiguous title,
category and available date match; it does not create the siblings again.

MongoDB is currently standalone. Multi-record writes use compensating deletes,
not a transaction; crash/network ambiguity can leave valid partial records.
Original files are retained when the database outcome is uncertain. Optimistic
version checks prevent a stale reprocess replacing a newer edit. A future replica
set should use transactions for batch publication. Re-upload is not idempotent.

## Reviewed legacy repair

Back up MongoDB, then run `split-study -id <reviewed-id>` as a dry run. Add
`-apply` only for a verified CBC + microalbumin record. The command accepts a
restricted set of previously reviewed marker identities, preserves all numeric
values/references and makes no external AI calls. Original combined transcription
and review remain recoverable from the backup; two independent reviews are
generated locally. A second apply rejects an already separated record.

Validation: `go test ./...`, `go vet ./...`. Tests cover multiple study types,
printed references, rejecting mixed/invalid groups, safe original-file copies,
failed-write cleanup and order-independent reprocess matching. These tests do not
prove perfect recognition of arbitrary photographs or clinical correctness.
