# Module dependencies

`internal/svc` is the process-wide composition root for application wiring. It loads no configuration itself: `main` supplies the validated `config.Config`, and the context creates one shared Docker client, one image-update cache, and the container, image, backup, port-settings, and icon services. It also connects image operations to the task store's read-only protected-image view so active or unresolved task facts can block destructive image cleanup. Update logic passes the resolved `config.UpdateConfig` by value to functions in `internal/updater`; no updater instance is stored in the context. Legacy fields and constructors remain available for compatibility with existing handlers and tests.

```text
handler ──> logic ──> container / image / backup / updater / settings
   │          │
   └──────────┴────> svc (construction and legacy constructor adapters)

svc ──> config, Docker client, task store, operation coordinator, module image cache, business services, settings, legacyprogress
container ──> Docker client, module image cache, domain types, settings port identity types/interface, operation coordinator
image ──> Docker client, domain types, protected-image reader interface
backup ──> Docker client, domain types, backup-compose encoder
updater ──> config values and HTTP/archive primitives
settings ──> config values and domain types
legacyprogress ──> legacy progress data only
```

Business services do not import `svc`, HTTP response types, or the legacy `utiles` package. Logic constructors keep their historical `*svc.ServiceContext` signatures for source compatibility, immediately extract the required service or scalar configuration, and retain no context pointer. Production handlers receive services assembled once by `svc`; legacy utility entry points delegate to those services. The icon HTTP handlers use the shared icon service when available and construct a temporary compatibility service for literal contexts in tests.

`utiles` remains as a compatibility surface for existing callers. Container operations, image listing/removal, backup export/list/restore/path operations, program update/version fetch, and port-override file access delegate to the corresponding business package. `backup_compose` remains a focused format encoder. Image update checks continue to use the existing shared `module.ImageUpdateData` cache; list requests do not construct a replacement cache. `svc` adapts the task store's protected image facts to the image package's reader interface, keeping the task package out of image business logic.

Task execution persistence and HTTP task routes are separate from the legacy progress map. The service context composes the persistent task store, runner, startup recovery, and controlled shutdown/readiness lifecycle. Existing progress-map APIs remain as a compatibility façade for older callers; new task-aware operations persist task records and expose them through the task routes. The image protection adapter derives image IDs and references from protected records and private snapshots inside svc, and exposes only these protection facts to image business logic.
