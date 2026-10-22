# API contract

All `/api` business routes except login require the configured JWT. Responses
keep the legacy `code`, `msg`, and `data` envelope. Errors use a legal HTTP
status and add `errorCode` and `requestId`; `X-Request-ID` is a UUID correlation
header. A valid incoming UUID is preserved in canonical form; invalid or
missing values are replaced with a server-generated UUID.

## Asynchronous tasks

An accepted task response means the task record was durably created and queued;
it does not mean the operation completed. Accepted responses include `taskID`;
the batch, image-cleanup, and program-update endpoints also include
`status: "queued"`, while backup, restore, and container-update responses
expose only `taskID`. Query an
individual task with `GET /api/progress/:taskid`, or list/filter tasks with
`GET /api/tasks`. A task
with `status: "failed"` is still a successful query response; inspect the task
status and diagnostics rather than treating the query itself as failed.

`POST /api/containers/batch` accepts JSON with `action` (`start`, `stop`,
`restart`, or `update`) and `containerIDs`. For `update`, the optional
`deleteOldContainer` boolean overrides the configured default; omission keeps
that default. The service de-duplicates IDs and accepts at most 100 targets. It
returns the parent task ID after durable acceptance and records per-target
success, failure, and skip results. The batch route is JWT protected.

`POST /api/images/cleanup` accepts `{"policy":"dangling"}` or
`{"policy":"unused"}` and returns a queued task ID. It is JWT protected;
per-image results are available through the task query endpoints.

`POST /api/container/backup` and `POST /api/container/backup2compose` accept
export tasks and return after queue acceptance. The legacy `GET` export routes
retain their synchronous completion semantics. `POST
/api/container/backups/restore` returns a task ID; restore validates the whole
input before Docker changes and reports per-item outcomes through the task.

`PUT /api/program` returns an accepted task ID. Online handoff leaves the task
running with a pending-install marker until the new process proves its binary
identity and HTTP readiness. The response never represents installation
completion.

## Batch request example

```json
{
  "action": "stop",
  "containerIDs": ["container-id-1", "container-id-2"]
}
```

For an update, each target uses its inspected container name and current image
configuration. Set `deleteOldContainer` explicitly only when overriding the
configured cleanup preference.
