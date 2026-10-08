# Host methods

Generated from the router by `go test ./internal/rpc/conformance -update`. Each is a JSON-RPC request whose method is the heading; the result is always an object.
Fields are additive only: a breaking change is a new method.

## `actions.list`

Params:

- `path` string

Result:

- `path` string
- `revision` int
- `actions` [object{Action}]
- `runs` [object{ActionRun}]

## `actions.output`

Params:

- `run_id` string

Result:

- `text` string
- `truncated` bool

## `actions.run`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `name` string

Result:

- `exit_code` int
- `stdout` string
- `stderr` string
- `duration_ms` int

## `actions.start`

Params:

- `path` string
- `name` string

Result:

- `run` object{ActionRun}

## `actions.stop`

Params:

- `run_id` string

Result:

- `ok` bool

## `browser.pending`

Params:

- none

Result:

- `requests` [object{Request}]

## `browser.respond`

Params:

- `request_id` string
- `result` object{Result}

Result:

- `ok` bool

## `builds.app_numbers`

Params:

- `project` string
- `path` string
- `target` string

Result:

- `version` string
- `build_number` string

## `builds.apps`

Params:

- `project` string

Result:

- `apps` [object{App}]
- `ios_supported` bool
- `install_port` int

## `builds.cancel`

Params:

- `build_id` string

Result:

- `ok` bool

## `builds.delete`

Params:

- `build_id` string

Result:

- `ok` bool

## `builds.get`

Params:

- `build_id` string

Result:

- `build` object{Build}

## `builds.install_link`

Params:

- `build_id` string

Result:

- `url` string
- `page_url` string
- `expires_at` int

## `builds.list`

Params:

- `project` string

Result:

- `builds` [object{Build}]

## `builds.log`

Params:

- `build_id` string
- `offset` int

Result:

- `data` base64 (nullable)
- `next_offset` int
- `done` bool

## `builds.publish`

Params:

- `request_id` string
- `project` string
- `path` string
- `target` string
- `version` string
- `build_number` string

Result:

- `publication` object{Publication}

## `builds.publish_cancel`

Params:

- `publication_id` string

Result:

- `ok` bool

## `builds.publish_list`

Params:

- `project` string
- `path` string
- `target` string

Result:

- `publications` [object{Publication}]

## `builds.publish_log`

Params:

- `publication_id` string
- `offset` int

Result:

- `data` base64 (nullable)
- `next_offset` int
- `done` bool

## `builds.publish_status`

Params:

- `publication_id` string

Result:

- `publication` object{Publication}

## `builds.start`

Params:

- `project` string
- `platform` string
- `path` string
- `target` string
- `configuration` string
- `method` string
- `version` string
- `build_number` string

Result:

- `build` object{Build}

## `chats.delete`

Params:

- `chat_id` string

Result:

- `ok` bool

## `chats.handoff`

Params:

- `chat_id` string

Result:

- `command` string

## `chats.info`

Params:

- `chat_id` string

Result:

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

## `chats.list`

Params:

- `cwd` string
- `cwds` [string] (nullable)
- `resolved` bool (nullable)
- `agent` string
- `sort` string
- `order` string
- `search` string
- `limit` int
- `refresh` bool
- `cursor` string

Result:

- `chats` [object{Chat}]
- `next_cursor` string
- `host_id` string

## `chats.messages`

Params:

- `chat_id` string
- `since_idx` int
- `limit` int
- `tail` bool
- `before_idx` int (nullable)

Result:

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

## `chats.neighbors`

Params:

- `cwd` string
- `cwds` [string] (nullable)
- `resolved` bool (nullable)
- `agent` string
- `sort` string
- `order` string
- `search` string
- `chat_id` string

Result:

- `previous` object{Chat} (nullable)
- `next` object{Chat} (nullable)
- `host_id` string

## `chats.queue`

Params:

- `chat_id` string

Result:

- `queue_rev` int
- `queued` [object{QueuedTurn}]

## `chats.resolve`

Params:

- `chat_id` string
- `resolved` bool

Result:

- `chat` object{Chat}

## `chats.send`

Params:

- `chat_id` string
- `turn` object{Turn}
- `steer` bool

Result:

- `turn_id` string
- `state` string
- `steered` bool

## `chats.start`

Detached: runs to completion if the caller disconnects.

Params:

- `turn` object{Turn}
- `path` string
- `agent` string
- `title` string

Result:

- `turn_id` string
- `state` string

## `chats.stop`

Params:

- `chat_id` string

Result:

- `ok` bool

## `chats.subagent`

Params:

- `chat_id` string
- `call_id` string

Result:

- `chat_id` string
- `call_id` string
- `kind` string
- `name` string
- `events` [object{Message}]
- `tools` [object{ToolDetail}]
- `host_id` string

## `chats.subscribe`

Params:

- `chat_id` string
- `since_idx` int

Result:

- `epoch` string
- `revision` int
- `streaming` object{Streaming} (nullable)
- `approval` object{Approval} (nullable)

## `chats.sync`

Params:

- `epoch` string
- `since` int

Result:

- `epoch` string
- `rev` int
- `more` bool
- `upsert` [object{Chat}]
- `delete` [string]

## `chats.tools_subscribe`

Params:

- `chat_id` string
- `call_ids` [string]

Result:

- `chat_id` string
- `tools` [object{ToolDetail}]

## `chats.tools_unsubscribe`

Params:

- `chat_id` string
- `call_ids` [string]

Result:

- `ok` bool

## `chats.unsubscribe`

Params:

- none

Result:

- `ok` bool

## `chats.update`

Params:

- `chat_id` string
- `title` string (nullable)

Result:

- `chat` object{Chat}

## `cloud.env.list`

Params:

- `provider` string
- `team_id` string
- `project_id` string

Result:

- `vars` [object{Var}]
- `targets` [string]

## `cloud.env.remove`

Detached: runs to completion if the caller disconnects.

Params:

- `provider` string
- `team_id` string
- `project_id` string
- `id` string
- `deploy` bool

Result:

- `ok` bool

## `cloud.env.set`

Detached: runs to completion if the caller disconnects.

Params:

- `provider` string
- `team_id` string
- `project_id` string
- `id` string
- `key` string
- `value` string
- `targets` [string] (nullable)
- `git_branch` string
- `secret` bool
- `deploy` bool

Result:

- `id` string
- `key` string
- `secret` bool
- `targets` [string]
- `git_branch` string

## `cloud.env.value`

Params:

- `provider` string
- `team_id` string
- `project_id` string
- `id` string

Result:

- `value` string

## `cloud.projects.list`

Params:

- `cwd` string

Result:

- `cwd` string
- `projects` [object{Project}]
- `issues` [object{Issue}]
- `needs_setup` [string]

## `devices.list`

Params:

- none

Result:

- `devices` [object{Device}]

## `devices.register_push`

Paired devices only: refused at the machine.

Params:

- `token` string
- `environment` string
- `install_id` string (omitted when empty)
- `at` int (omitted when empty)
- `chat_id` string
- `kind` string

Result:

- `ok` bool

## `devices.revoke`

Detached: runs to completion if the caller disconnects.

Params:

- `id` string

Result:

- `ok` bool

## `env.pending`

Params:

- none

Result:

- `requests` [object{Request}]

## `env.provide`

Params:

- `request_id` string
- `approved` bool
- `results` object (nullable)

Result:

- `ok` bool

## `env.read`

Params:

- `handles` [string] (nullable)

Result:

- `results` object (nullable)

## `env.sources.list`

Params:

- none

Result:

- `sources` [object{Source}]

## `env.sources.remove`

Params:

- `handle` string

Result:

- `ok` bool

## `env.sources.set`

Params:

- `handle` string
- `path` string

Result:

- `source` object{Source}

## `forward.fetch`

Params:

- `port` int
- `method` string
- `path` string
- `headers` object (omitted when empty) (nullable)
- `body` base64 (omitted when empty) (nullable)

Result:

- `status` int
- `headers` object (omitted when empty) (nullable)
- `body` base64 (omitted when empty) (nullable)
- `truncated` bool (omitted when empty)

## `forward.pipe_close`

Params:

- `pipe_id` string

Result:

- `ok` bool

## `forward.pipe_open`

Params:

- `pipe_id` string
- `port` int
- `data` base64 (nullable)

Result:

- `ok` bool

## `forward.pipe_send`

Params:

- `pipe_id` string
- `data` base64 (nullable)

Result:

- `ok` bool

## `fs.delete`

Params:

- `path` string

Result:

- `ok` bool

## `fs.list`

Params:

- `path` string
- `dirs_only` bool
- `hidden` bool
- `extensions` [string]
- `within` string

Result:

- `path` string
- `entries` [object{Entry}]
- `truncated` bool

## `fs.mkdir`

Params:

- `path` string

Result:

- `ok` bool

## `fs.read`

Params:

- `path` string
- `mod_time` int
- `within` string
- `extensions` [string] (nullable)

Result:

- `path` string
- `content` base64
- `size` int
- `mod_time` int
- `binary` bool
- `unchanged` bool

## `fs.rename`

Params:

- `path` string
- `new_path` string

Result:

- `ok` bool

## `fs.replace`

Params:

- `path` string
- `old` string
- `new` string
- `mod_time` int
- `within` string
- `extensions` [string] (nullable)

Result:

- `path` string
- `content` base64
- `size` int
- `mod_time` int
- `binary` bool

## `fs.search`

Params:

- `path` string
- `query` string
- `mode` string
- `limit` int
- `case_sensitive` bool

Result:

- `results` [object{SearchFile}]
- `truncated` bool

## `fs.stop`

Params:

- none

Result:

- `ok` bool

## `fs.watch`

Params:

- `paths` [string]
- `active` [string]
- `resync` bool (omitted when empty)

Result:

- `ok` bool

## `fs.write`

Params:

- `path` string
- `content` base64
- `create` bool
- `mod_time` int
- `within` string
- `extensions` [string] (nullable)

Result:

- `path` string
- `content` base64
- `size` int
- `mod_time` int
- `binary` bool

## `git.branches`

Params:

- `path` string

Result:

- `path` string
- `branches` [object{Branch}]

## `git.changes`

Params:

- `path` string

Result:

- `path` string
- `files` [object{FileChange}]

## `git.commit_push`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `message` string

Result:

- `commit` string
- `branch` string
- `pushed` bool
- `shipped` object{Shipped} (omitted when empty) (nullable)

## `git.create_branch`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `name` string
- `from_ref` string

Result:

- `ok` bool

## `git.diff`

Params:

- `path` string

Result:

- `working` object{ChangeSet}

## `git.patch`

Params:

- `path` string
- `base` string
- `head` string
- `file` string

Result:

- `base` string
- `head` string (omitted when empty)
- `files` [object{FileChange}] (omitted when empty) (nullable)
- `text` string (omitted when empty)
- `truncated` bool (omitted when empty)

## `git.pull`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string

Result:

- `ok` bool

## `git.reset_hard`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `branch` string

Result:

- `ok` bool

## `git.status`

Params:

- `paths` [string]

Result:

- `projects` [object{Status}]

## `git.switch_branch`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `branch` string

Result:

- `ok` bool

## `github.avatar`

Params:

- `if_none_match` string

Result:

- `login` string (omitted when empty)
- `url` string (omitted when empty)
- `content_hash` string (omitted when empty)
- `content_type` string (omitted when empty)
- `bytes` base64 (omitted when empty) (nullable)
- `not_modified` bool (omitted when empty)

## `github.clone`

Detached: runs to completion if the caller disconnects.

Params:

- `name_with_owner` string

Result:

- `name_with_owner` string
- `path` string
- `existed` bool (omitted when empty)

## `github.pr_create`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `title` string
- `body` string
- `base` string
- `draft` bool

Result:

- `number` int
- `url` string
- `state` string
- `title` string
- `base` string
- `head` string
- `merged` bool (omitted when empty)
- `existed` bool (omitted when empty)
- `shipped` object{Shipped} (omitted when empty) (nullable)

## `github.publish`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `name` string
- `private` bool

Result:

- `name_with_owner` string
- `url` string
- `branch` string

## `github.repos`

Params:

- `owner` string
- `query` string
- `limit` int

Result:

- `repos` [object{Repo}]

## `host.claim`

Paired devices only: refused at the machine.

Params:

- `uid` string
- `nonce` string

Result:

- `host_id` string
- `public_key` base64 (nullable)
- `signature` base64 (nullable)

## `host.release`

Local only: refused to a paired device.

Params:

- none

Result:

- `ok` bool

## `host.set_stops_at`

Params:

- `stops_at` json

Result:

- `provider` string
- `stops_at` json

## `host.setup`

Params:

- `refresh` bool

Result:

- `host` object{Release}
- `tools` [object{Install}]
- `agents` [object{Agent}]
- `github` object{CloneFolder}

## `host.status`

Params:

- none

Result:

- `version` string
- `executable` string
- `uptime_seconds` int
- `relay_connected` bool
- `update_failed` string (omitted when empty)
- `battery` object{Battery} (omitted when empty) (nullable)
- `cloud` object{Cloud} (omitted when empty) (nullable)
- `network` object{Network}
- `capabilities` [string] (nullable)

## `host.update`

Detached: runs to completion if the caller disconnects.

Params:

- `now` bool

Result:

- `from` string
- `to` string (omitted when empty)
- `busy` object{Busy} (omitted when empty) (nullable)

## `limits.read`

Params:

- none

Result:

- `agents` [object{Usage}]

## `limits.reset`

Detached: runs to completion if the caller disconnects.

Params:

- `kind` string
- `credit_id` string

Result:

- `outcome` string

## `mcp.connect`

Params:

- `probe_id` string
- `api_key` string (omitted when empty)

Result:

- `server` object{Server}

## `mcp.list`

Params:

- `project` string

Result:

- `servers` [object{Server}]
- `builtins` [object{Builtin}]
- `installed` [object{Installed}]
- `root` string (omitted when empty)

## `mcp.oauth_finish`

Detached: runs to completion if the caller disconnects.

Params:

- `probe_id` string
- `code` string
- `state` string

Result:

- `server` object{Server}

## `mcp.oauth_start`

Params:

- `probe_id` string
- `client_id` string (omitted when empty)
- `loopback` bool

Result:

- `authorize_url` string
- `state` string

## `mcp.probe`

Params:

- `url` string
- `label` string (omitted when empty)
- `project` string

Result:

- `probe_id` string
- `url` string
- `auth` string
- `needs_manual_client` bool
- `registration_error` string (omitted when empty)

## `mcp.remove`

Params:

- `id` string

Result:

- `ok` bool

## `mcp.rename`

Params:

- `id` string
- `label` string

Result:

- `ok` bool

## `mcp.set_enabled`

Params:

- `id` string
- `project` string
- `enabled` bool

Result:

- `ok` bool

## `pair.begin`

Local only: refused to a paired device.

Params:

- none

Result:

- `invite` object{Invite}
- `qr` string

## `pair.complete`

Unpaired: reachable before pairing, guarded by the pairing code.

Params:

- `device_id` string
- `public_key` base64 (nullable)
- `label` string
- `platform` string
- `proof` base64 (nullable)

Result:

- `group_id` string
- `host_id` string
- `host_public` base64 (nullable)
- `host_proof` base64 (nullable)
- `host_label` string

## `pair.reusable`

Local only: refused to a paired device.

Params:

- `hours` int

Result:

- `invite` object{Invite}
- `qr` string

## `pair.reusable_revoke`

Local only: refused to a paired device.

Params:

- none

Result:

- `ok` bool

## `pair.status`

Local only: refused to a paired device.

Params:

- none

Result:

- `pending` bool
- `expires_at` int (omitted when empty)
- `reusable_expires_at` int (omitted when empty)

## `ports.kill`

Detached: runs to completion if the caller disconnects.

Params:

- `port` int
- `force` bool

Result:

- `killed_pids` [int]
- `command` string (omitted when empty)

## `ports.list`

Params:

- `dir` string

Result:

- `ports` [object{Port}]

## `projects.add`

Params:

- `path` string

Result:

- `path` string

## `projects.create`

Params:

- `name` string
- `parent` string

Result:

- `path` string

## `projects.detect_icon`

Params:

- `path` string
- `if_none_match` string

Result:

- `path` string
- `source` string
- `content_hash` string
- `content_type` string
- `bytes` base64 (omitted when empty) (nullable)
- `not_modified` bool (omitted when empty)
- `remote` string (omitted when empty)

## `projects.list`

Params:

- none

Result:

- `projects` [object{Project}]

## `projects.pin`

Params:

- `path` string
- `pinned` bool

Result:

- `path` string
- `chat_count` int
- `repo_owner` string
- `repo_name` string
- `kind` string
- `icon_hash` string
- `diff_available` bool
- `files_changed` int
- `additions` int
- `deletions` int
- `activity_at` int
- `last_message_at` int
- `display_name` string
- `pinned_at` int (omitted when empty) (nullable)
- `host_id` string

## `projects.rename`

Params:

- `path` string
- `name` string

Result:

- `path` string
- `chat_count` int
- `repo_owner` string
- `repo_name` string
- `kind` string
- `icon_hash` string
- `diff_available` bool
- `files_changed` int
- `additions` int
- `deletions` int
- `activity_at` int
- `last_message_at` int
- `display_name` string
- `pinned_at` int (omitted when empty) (nullable)
- `host_id` string

## `schedules.delete`

Params:

- `id` string

Result:

- `ok` bool

## `schedules.list`

Params:

- none

Result:

- `schedules` [object{Row}]

## `schedules.save`

Params:

- `id` string
- `title` string
- `prompt` string
- `path` string
- `agent` string
- `config` object{TurnConfig}
- `frequency` string
- `hour` int
- `minute` int
- `weekday` int
- `timezone` string
- `enabled` bool

Result:

- `id` string
- `title` string
- `prompt` string
- `path` string
- `agent` string
- `config` object{TurnConfig}
- `frequency` string
- `hour` int
- `minute` int
- `weekday` int
- `timezone` string
- `enabled` bool
- `host_id` string
- `next_run_at` int

## `terminals.attach`

Params:

- `session_id` string

Result:

- `session_id` string
- `cwd` string
- `shell` string
- `pid` int
- `cols` int
- `rows` int
- `created_at_ms` int
- `buffered_output` base64 (nullable)

## `terminals.close`

Params:

- `session_id` string

Result:

- `ok` bool

## `terminals.create`

Params:

- `cwd` string
- `cols` int
- `rows` int

Result:

- `session_id` string
- `cwd` string
- `shell` string
- `pid` int
- `cols` int
- `rows` int
- `created_at_ms` int

## `terminals.detach`

Params:

- `session_id` string

Result:

- `ok` bool

## `terminals.input`

Params:

- `session_id` string
- `data` base64 (nullable)

Result:

- `ok` bool

## `terminals.list`

Params:

- `cwd` string

Result:

- `sessions` [object{Info}]

## `terminals.resize`

Params:

- `session_id` string
- `cols` int
- `rows` int

Result:

- `ok` bool

## `terminals.subscribe`

Params:

- `cwd` string

Result:

- `ok` bool

## `terminals.unsubscribe`

Params:

- `cwd` string

Result:

- `ok` bool

## `tools.install`

Detached: runs to completion if the caller disconnects.

Params:

- `kind` string

Result:

- `kind` string
- `ran` string (omitted when empty)
- `ok` bool
- `error` string (omitted when empty)
- `output` string (omitted when empty)
- `version` string (omitted when empty)

## `tools.login_cancel`

Params:

- `kind` string

Result:

- `ok` bool

## `tools.login_complete`

Detached: runs to completion if the caller disconnects.

Params:

- `kind` string
- `code` string

Result:

- `ok` bool

## `tools.login_start`

Params:

- `kind` string

Result:

- `url` string
- `style` string
- `user_code` string (omitted when empty)
- `expires_at_ms` int

## `tools.logins`

Params:

- none

Result:

- `waiting` [string]

## `tools.logout`

Detached: runs to completion if the caller disconnects.

Params:

- `kind` string

Result:

- `kind` string
- `ran` string (omitted when empty)
- `ok` bool
- `error` string (omitted when empty)
- `output` string (omitted when empty)
- `version` string (omitted when empty)

## `tools.uninstall`

Detached: runs to completion if the caller disconnects.

Params:

- `kind` string

Result:

- `kind` string
- `ran` string (omitted when empty)
- `ok` bool
- `error` string (omitted when empty)
- `output` string (omitted when empty)
- `version` string (omitted when empty)

## `tools.update`

Detached: runs to completion if the caller disconnects.

Params:

- `kind` string

Result:

- `updated` [object{UpdateResult}]

## `tunnels.close`

Params:

- `slug` string

Result:

- `ok` bool

## `tunnels.list`

Params:

- none

Result:

- `tunnels` [object{Tunnel}]
- `connected` bool

## `tunnels.open`

Paired devices only: refused at the machine.

Params:

- `slug` string
- `port` int
- `expires_at` int

Result:

- `tunnel` object{Tunnel}

## `turns.edit_queued`

Params:

- `turn_id` string
- `prompt` string

Result:

- `ok` bool

## `turns.get`

Params:

- `turn_id` string

Result:

- `turn_id` string
- `chat_id` string
- `agent` string
- `cwd` string
- `prompt` string
- `state` string
- `model` string (omitted when empty)
- `session_id` string (omitted when empty)
- `provider_stop_reason` string (omitted when empty) (nullable)
- `error` string (omitted when empty)
- `usage` object{Usage} (omitted when empty) (nullable)
- `queued_at` json
- `started_at` json (omitted when empty) (nullable)
- `ended_at` json (omitted when empty) (nullable)
- `event_count` int
- `approval` object{Approval} (omitted when empty) (nullable)

## `turns.list`

Params:

- none

Result:

- `turns` [object{TurnStatus}] (nullable)

## `turns.remove_queued`

Params:

- `turn_id` string

Result:

- `ok` bool

## `turns.respond`

Params:

- `turn_id` string
- `call_id` string
- `answer` json (nullable)

Result:

- `ok` bool

## `turns.send_queued`

Params:

- `turn_id` string

Result:

- `turn_id` string
- `chat_id` string
- `agent` string
- `cwd` string
- `prompt` string
- `state` string
- `model` string (omitted when empty)
- `session_id` string (omitted when empty)
- `provider_stop_reason` string (omitted when empty) (nullable)
- `error` string (omitted when empty)
- `usage` object{Usage} (omitted when empty) (nullable)
- `queued_at` json
- `started_at` json (omitted when empty) (nullable)
- `ended_at` json (omitted when empty) (nullable)
- `event_count` int
- `approval` object{Approval} (omitted when empty) (nullable)

## `turns.stop`

Params:

- `turn_id` string

Result:

- `ok` bool

## `usage.daily`

Params:

- none

Result:

- `host_id` string
- `captured_at_ms` int
- `time_zone` string
- `buckets` [object{Bucket}]

## `usage.history`

Params:

- `since_ms` int
- `until_ms` int

Result:

- `host_id` string
- `captured_at_ms` int
- `complete` bool
- `homes` [object{Home}]
- `buckets` [object{HourBucket}]
- `sessions` [object{AgentCount}]
- `pricing` object{PricingStatus}

## `vercel.deployments`

Params:

- `team_id` string
- `project_id` string
- `environment` string
- `state` string
- `cursor` string
- `limit` int

Result:

- `deployments` [object{DeploymentSummary}]
- `next_cursor` string

## `vercel.logs`

Params:

- `team_id` string
- `project_id` string
- `environment` string
- `level` string
- `status_code` string
- `window_minutes` int
- `cursor` string
- `limit` int

Result:

- `entries` [object{LogEntry}]
- `next_cursor` string
