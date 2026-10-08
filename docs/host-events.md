# Host events

Generated from the Go event catalog by `go test ./internal/emit -update`. Each is a JSON-RPC notification whose method is the heading and whose params are the fields.
Fields are additive only: a breaking change is a new method.

## `browser.request`

Signal: not replayed; a missed one is recovered by refetching.

- `request_id` string
- `turn_id` string
- `chat_id` string
- `cwd` string
- `expires_at` string
- `action` object{Action}

## `chats.appended`

Signal: not replayed; a missed one is recovered by refetching.

- `chat_id` string
- `events` [object{Message}]
- `host_id` string (omitted when empty)
- `cwd` string
- `agent` string
- `generation` int
- `event_count` int
- `next_idx` int
- `first_idx` int
- `has_before` bool

## `chats.approval`

Stateful, key `approval`: the last one is handed to a device joining the room; `revision` orders them within an `epoch`.

- `chat_id` string
- `turn_id` string
- `approval` object{Approval} (nullable)
- `epoch` string
- `revision` int

## `chats.attention`

Signal: not replayed; a missed one is recovered by refetching.

- `chat_id` string
- `turn_id` string
- `kind` string
- `approval` object{Approval} (omitted when empty) (nullable)
- `call_id` string (omitted when empty)
- `error` string (omitted when empty)
- `reply` string (omitted when empty)
- `answerable` bool

## `chats.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `id` string
- `host_id` string (omitted when empty)
- `agent` string
- `title` string
- `cwd` string
- `created_at` int
- `updated_at` int
- `activity_at` int
- `event_count` int
- `generation` int
- `status` string
- `status_at` int
- `last_turn_started_at` int (nullable)
- `last_turn_finished_at` int (nullable)
- `last_reply` string (omitted when empty)
- `last_user_message_preview` string (omitted when empty)
- `rev` int
- `resolved_at` int (omitted when empty) (nullable)
- `context_used` int (omitted when empty)
- `context_size` int (omitted when empty)
- `model` string (omitted when empty)
- `model_label` string (omitted when empty)
- `permission_mode` string (omitted when empty)
- `mode` string (omitted when empty)
- `reasoning_level` string (omitted when empty)
- `fast_mode` bool (omitted when empty) (nullable)
- `voice_handle` object{VoiceHandle} (omitted when empty) (nullable)
- `queued_count` int (omitted when empty)
- `queue_rev` int (omitted when empty)
- `match` string (omitted when empty)

## `chats.removed`

Signal: not replayed; a missed one is recovered by refetching.

- `chat_id` string

## `chats.streaming`

Stateful, key `streaming`: the last one is handed to a device joining the room; `revision` orders them within an `epoch`.

- `chat_id` string
- `turn_id` string
- `text` string
- `offset` int (omitted when empty) (nullable)
- `done` bool
- `started_at` int (omitted when empty)
- `first_frame_at` int (omitted when empty)
- `ended_at` int (omitted when empty)
- `stop_reason` string (omitted when empty)
- `epoch` string
- `revision` int

## `chats.tool`

Signal: not replayed; a missed one is recovered by refetching.

- `chat_id` string
- `call_id` string
- `name` string
- `state` string
- `input` json (omitted when empty) (nullable)
- `output` string (omitted when empty)
- `output_bytes` int (omitted when empty)

## `devices.changed`

Signal: not replayed; a missed one is recovered by refetching.


## `env.request`

Signal: not replayed; a missed one is recovered by refetching.

- `request_id` string
- `host_label` string
- `path` string
- `handles` [string]
- `services` [string]
- `expires_at` string
- `state` string

## `forward.pipe_closed`

Signal: not replayed; a missed one is recovered by refetching.

- `pipe_id` string
- `reason` string

## `forward.pipe_data`

Signal: not replayed; a missed one is recovered by refetching.

- `pipe_id` string
- `data` base64 (nullable)

## `fs.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `path` string
- `changes` [object{FileChange}]
- `truncated` bool (omitted when empty)

## `git.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `projects` [object{Project}]

## `host.power`

Signal: not replayed; a missed one is recovered by refetching.

- `present` bool
- `percent` int
- `charging` bool
- `plugged_in` bool

## `host.setup_changed`

Signal: not replayed; a missed one is recovered by refetching.

- `host` object{Release}
- `tools` [object{Install}]
- `agents` [object{Agent}]
- `github` object{CloneFolder}

## `mcp.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `servers` [object{Server}]
- `builtins` [object{Builtin}]

## `projects.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `projects` [object{Project}]
- `removed` [string]

## `schedules.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `schedules` [object{Row}]

## `services.status`

Signal: not replayed; a missed one is recovered by refetching.

- `path` string
- `root` string
- `revision` int
- `idle_deadline` string (omitted when empty)
- `services` [object{Service}]

## `terminals.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `cwd` string
- `sessions` [object{Info}]

## `terminals.exit`

Signal: not replayed; a missed one is recovered by refetching.

- `session_id` string
- `exit_code` int

## `terminals.output`

Signal: not replayed; a missed one is recovered by refetching.

- `session_id` string
- `data` base64 (nullable)

## `tunnels.changed`

Signal: not replayed; a missed one is recovered by refetching.

- `tunnels` [object{Tunnel}]
- `connected` bool
