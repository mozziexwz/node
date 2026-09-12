# Task and executor API

Browser requests use authenticated session cookies and `X-CSRF-Token` for writes. JSON uses camelCase. All endpoints return `Cache-Control: no-store`; errors are `{ "error": "中文说明" }`.

- `GET /api/tasks`: `{tasks: Task[], limits: {deploy, relay, dd, fingerprint}}`. Each limit has `minutes,count,remaining,nextAt` (Unix ms, zero when not blocked).
- `POST /api/fingerprints`: `{host: "公网IP", port: 22}`. No password. A live executor performs an actual SSH host-key handshake; response `{host,port,fingerprint,algorithm,checkedAt}`. Wait up to 28 seconds. The user must confirm this fingerprint. Each matching user/host/port proof is valid for 30 minutes. Front and relay each need their own check.
- `POST /api/tasks`: requires `Idempotency-Key` (8–128 chars). Body `{kind:"deploy"|"relay"|"dd",ssh:{host,port,user:"root",password,fingerprint},mode?:"fresh"|"repair",clientConfig?:object,front?:SSH,dd?:{confirmErase:true,portMode:"keep"|"new",newPort?,passwordMode:"keep"|"new",newPassword?}}`. Deploy defaults should be selected by the UI as `fresh`. JSON is an object, not a string. Only one profile, server and TCP port. `front` adds a second server and does not replace `ssh`. Returns a Task with HTTP 202 (new) or 200 (idempotent repeat).
- `GET /api/tasks/{id}`: task metadata and transient `configAvailable` boolean. Owners and administrators may read metadata; another user's configuration is never available to an administrator. Poll until terminal.
- `GET /api/tasks/{id}/config`: raw client JSON, available for 10 minutes in server RAM, login/ownership enforced. Save into encrypted browser IndexedDB; filename `直连.json` for deploy, `自备中转.json` for relay. Server restarts discard this temporary copy. Historical task metadata stays available without a new email gate.
- `GET /api/admin/tasks`: `{tasks: Task[]}`, sanitized audit only.

Task fields: `id,userId,kind,host,mode,state,phase,message,createdAt,updatedAt,configAvailable,health?,hops?`. `health` separates `service,localSelfTest,publicTCP,game`. `hops` use `fromHost,fromPort,toHost,toPort`. A TCP check never means a game session was verified. States: `queued,running,succeeded,failed,executed,unknown,interrupted`. `executed` is only used for DD preparation + submitted reboot + expected disconnect; it does not claim the OS installation finished. Unknown or interrupted tasks are never automatically retried. A user retry is a new confirmed task with the same chosen policy and a new idempotency key.

Maintenance mode blocks new deploy, relay, DD and paid-front execution tasks for all roles, including administrators. Existing metadata and owner-only temporary configuration remain readable; password-free SSH host-key probes remain available to signed-in users for diagnostics (their normal quota still applies). Already accepted tasks are not automatically cancelled.

## Executor administration

- `GET /api/admin/executors` returns `{executors:[{id,name,status,createdAt,lastSeenAt}]}`. `status` is `active` or `disabled`; recent heartbeat means `Date.now()-lastSeenAt < 90000`.
- `POST /api/admin/executors` accepts `{name:"Executor 1"}`, returns `{executor:{...},token:"only shown once"}`. Save token into protected `MSBOOST_EXECUTOR_TOKEN` environment file. It is stored as a hash in the database. It cannot authenticate relay agents.
- `PATCH /api/admin/executors/{id}` accepts `{name?,status?:"active"|"disabled"}`.
- `DELETE /api/admin/executors/{id}` revokes the token. Already delivered tasks cannot be recalled; a destructive task is not silently retried.
- Executor-only `GET /api/executor/next` uses `Authorization: Bearer ...`, long-polls for 25 seconds and returns 204 if empty. A task payload is handed out once, held only in memory, with a unique lease. Lost delivery or restarts interrupt the task rather than replay it.
- Executor-only `POST /api/executor/result` returns 200; duplicate/expired result leases return 409. Results can be retried; execution cannot.
- Executor-only `POST /api/executor/heartbeat` updates liveness every 20 seconds even while a long task is running.

The agent must reach the control server by HTTPS except loopback development. Run `msboost-agent --capability executor`; set `MSBOOST_SERVER_URL`, `MSBOOST_EXECUTOR_TOKEN`, `GOST_AMD64_URL`, `GOST_AMD64_SHA256`, `GOST_ARM64_URL`, `GOST_ARM64_SHA256`. GOST URLs must be pinned official `go-gost/gost` release archives, not `latest`. Remote free relay installation requires root SSH and systemd 247+ with `LoadCredential` support (Debian 12 supported). Missing Python 3, curl and tar are installed automatically on Debian/Ubuntu; other systems must provide these dependencies. Unconfigured architecture assets fail explicitly. Use a separate token and `--capability relay` for managed station relay agents.

## Internal paid front adapter

`TaskService.ProvisionFront(ctx,userID,ssh,targetHost,targetPort) (executor.Hop,error)` provisions a customer front server toward the actual allocated station entry. It checks live subscription and the user's previous SSH fingerprint proof; it does not consume a free-tool quota or require free-tool email policy. Caller owns the paid rule transaction and must revoke station rules on failure. Credentials remain short-lived memory. In-flight timeouts never count as verified completion.
