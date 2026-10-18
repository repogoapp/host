# Host methods

Generated from the router by `go test ./internal/rpc/conformance -update`. Each is a JSON-RPC request whose method is the heading; the result is always an object.
Fields are additive only: a breaking change is a new method.

## `actions.list`

Tool (read): A project's actions (the commands its actions.json declares) and their runs.

Live on: `actions.changed`

Params:

- `path` string: The project's folder

Result:

- `path` string
- `revision` int
- `actions` [object{Action}]
- `runs` [object{Run}]

## `actions.output`

Params:

- `run_id` string: The run's id, as actions.start or actions.list gives it

Result:

- `text` string
- `truncated` bool

## `actions.run`

Detached: runs to completion if the caller disconnects.

Tool (write): Run an action to the end and answer with its output.

Params:

- `path` string: The project's folder
- `name` string: The action's name, as actions.list gives it
- `secrets` object (omitted when empty) (nullable): Leave empty

Result:

- `exit_code` int
- `stdout` string
- `stderr` string
- `duration_ms` int

## `actions.start`

Tool (write): Start an action that keeps running, such as a dev server; answers with its run.

Params:

- `path` string: The project's folder
- `name` string: The action's name, as actions.list gives it
- `secrets` object (omitted when empty) (nullable): Leave empty

Result:

- `run` object{Run}

## `actions.stop`

Tool (write): Stop an action's run.

Params:

- `run_id` string: The run's id, as actions.start or actions.list gives it

Result:

- `ok` bool

## `agents.list`

Tool (read): The agents a chat in a project can start with, for chats.start: each one's models, default model, the efforts (reasoning_level) each model accepts, and its modes and permission modes.

Params:

- `project` string: The project's folder

Result:

- `agents` [object{AgentChoice}]

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

## `chats.delete`

Params:

- `chat_id` string: The chat's id, as chats.list gives it

Result:

- `ok` bool

## `chats.handoff`

Params:

- `chat_id` string: The chat's id, as chats.list gives it

Result:

- `command` string

## `chats.info`

Tool (read): One chat's row: title, project, status, last reply.

Live on: `chats.changed`, `chats.removed`

Params:

- `chat_id` string: The chat's id, as chats.list gives it

Result:

- `id` string
- `host_id` string (omitted when empty)
- `agent` string
- `title` string
- `cwd` string
- `created_at` int
- `updated_at` int
- `activity_at` int
- `status` string
- `status_at` int
- `last_turn_started_at` int (nullable)
- `last_turn_finished_at` int (nullable)
- `last_reply` string (omitted when empty)
- `last_user_message_preview` string (omitted when empty)
- `revision` int
- `resolved_at` int (omitted when empty) (nullable)
- `pinned_at` int (omitted when empty) (nullable)
- `parent_id` string (omitted when empty)
- `context_used` int (omitted when empty)
- `context_size` int (omitted when empty)
- `model` string (omitted when empty)
- `model_label` string (omitted when empty)
- `project_agent` string (omitted when empty)
- `project_agent_icon` string (omitted when empty)
- `permission_mode` string (omitted when empty)
- `mode` string (omitted when empty)
- `reasoning_level` string (omitted when empty)
- `fast_mode` bool (omitted when empty) (nullable)
- `voice_number` int (omitted when empty)
- `queued_count` int (omitted when empty)
- `queue_revision` int (omitted when empty)
- `match` string (omitted when empty)
- `match_index` int (omitted when empty) (nullable)

## `chats.list`

Tool (read): List the chats on this environment, newest activity first, filtered and sorted like the chat drawer. Empty cwd is every project.

Live on: `chats.changed`, `chats.removed`

Params:

- `cwd` string: A project's folder; empty is every project
- `cwds` [string] (nullable): Several projects' folders
- `resolved` bool (nullable): true: only resolved chats; false: only open ones; omit for both
- `agent` string: Only this agent's chats: claude, codex; empty is all
- `sort` string: activity (default), created or title
- `order` string: desc (default) or asc
- `search` string: Words a chat's title, prompts or replies must all hold
- `parent` string: Only the chats this chat's agent started; empty leaves those out
- `limit` int: Chats per page
- `refresh` bool: Re-read the agents' files first; slow, leave false
- `cursor` string: The previous page's next_cursor; empty starts over

Result:

- `chats` [object{Chat}]
- `next_cursor` string

## `chats.messages`

Tool (read): A page of one chat's messages. tail with a limit reads the newest; before_index pages older.

Live on: `chats.changed`

Params:

- `chat_id` string: The chat's id, as chats.list gives it
- `since_index` int: Messages after this index
- `limit` int: Messages per page
- `tail` bool: Read the newest messages
- `before_index` int (nullable): Messages before this index, to page older

Result:

- `chat_id` string
- `events` [object{Message}]
- `host_id` string (omitted when empty)
- `cwd` string
- `agent` string
- `project_agent` string (omitted when empty)
- `generation` int
- `event_count` int
- `next_index` int
- `first_index` int
- `has_before` bool

## `chats.neighbors`

Params:

- `cwd` string: A project's folder; empty is every project
- `cwds` [string] (nullable): Several projects' folders
- `resolved` bool (nullable): true: only resolved chats; false: only open ones; omit for both
- `agent` string: Only this agent's chats: claude, codex; empty is all
- `sort` string: activity (default), created or title
- `order` string: desc (default) or asc
- `search` string: Words a chat's title, prompts or replies must all hold
- `parent` string: Only the chats this chat's agent started; empty leaves those out
- `chat_id` string

Result:

- `previous` object{Chat} (nullable)
- `next` object{Chat} (nullable)
- `host_id` string

## `chats.pin`

Tool (write): Pin a chat to the top of the drawer, or unpin it.

Params:

- `chat_id` string: The chat's id, as chats.list gives it
- `pinned` bool: true pins; false unpins

Result:

- `chat` object{Chat}

## `chats.queue`

Params:

- `chat_id` string: The chat's id, as chats.list gives it

Result:

- `queue_revision` int
- `queued` [object{QueuedTurn}]

## `chats.resolve`

Tool (write): Resolve a chat (moves it to the done pile) or unresolve it.

Params:

- `chat_id` string: The chat's id, as chats.list gives it
- `resolved` bool: true resolves; false reopens

Result:

- `chat` object{Chat}

## `chats.send`

Tool (write): Send a message to a chat. It starts a turn, or queues behind the running one; steer adds it to the running turn where the agent allows it.

Params:

- `chat_id` string: The chat's id, as chats.list gives it
- `turn` object{Turn}: The message
- `steer` bool: Add it to the running turn instead of queueing it

Result:

- `turn_id` string
- `state` string
- `steered` bool

## `chats.start`

Detached: runs to completion if the caller disconnects.

Tool (write): Start a new chat in a project with its first message. Started by an agent, it runs under the calling chat, out of the drawer, with no broader permissions; child false opens a normal chat for the user instead.

Params:

- `turn` object{Turn}: The first message
- `cwd` string: The project's folder
- `agent` string: claude or codex
- `title` string: The chat's name; empty takes the message's opening words
- `project_agent` string (omitted when empty)
- `child` bool (omitted when empty) (nullable): For an agent: false opens a normal chat for the user; omit to start one under this chat

Result:

- `turn_id` string
- `state` string

## `chats.stop`

Tool (write): Stop a chat's running turn.

Params:

- `chat_id` string: The chat's id, as chats.list gives it

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
- `since_index` int

Result:

- `epoch` string
- `revision` int
- `streaming` object{Streaming} (nullable)
- `approval` object{Approval} (nullable)

## `chats.sync`

Params:

- `epoch` string
- `since_revision` int

Result:

- `epoch` string
- `revision` int
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

Tool (write): Rename a chat. The title is written to the agent's own files, so its CLI shows it too.

Params:

- `chat_id` string: The chat's id, as chats.list gives it
- `title` string (nullable): The new title

Result:

- `chat` object{Chat}

## `chats.wait`

Tool (read): Wait for a turn chats.start or chats.send returned to end, then return its state, its chat_id and the agent's last message. A timeout leaves it running; wait again.

Params:

- `turn_id` string: The turn_id chats.start or chats.send returned
- `timeout_ms` int: How long to wait; default 50000, at most 300000

Result:

- `turn_id` string
- `chat_id` string (omitted when empty)
- `state` string
- `reply` string (omitted when empty)
- `error` string (omitted when empty)

## `devices.hello`

Paired devices only: refused at the machine.

Params:

- `platform` string (omitted when empty)
- `kind` string (omitted when empty)
- `version` string (omitted when empty)
- `build` string (omitted when empty)
- `features` object (omitted when empty) (nullable)

Result:

- `ok` bool

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
- `grant` base64 (omitted when empty) (nullable)
- `granted_at` int (omitted when empty)
- `kind` string

Result:

- `ok` bool

## `devices.revoke`

Detached: runs to completion if the caller disconnects.

Params:

- `id` string

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
- `path` string

Result:

- `status` int

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

- `mod_time` int

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

## `fs.unwatch`

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

- `mod_time` int

## `git.branches`

Params:

- `path` string: The project's folder

Result:

- `branches` [object{Branch}]

## `git.changes`

Tool (read): One project's changed files, staged or not.

Live on: `git.changed`

Params:

- `path` string: The project's folder

Result:

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

## `git.create_branch`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string
- `name` string
- `from_ref` string

Result:

- `ok` bool

## `git.diff`

Tool (read): One project's uncommitted totals: files changed, lines added and removed. git.changes lists the files.

Live on: `git.changed`

Params:

- `path` string: The project's folder

Result:

- `files_changed` int
- `additions` int
- `deletions` int

## `git.patch`

Params:

- `path` string
- `base` string
- `head` string
- `file` string

Result:

- `files` [object{FileChange}] (omitted when empty) (nullable)
- `text` string (omitted when empty)
- `truncated` bool (omitted when empty)

## `git.pull`

Detached: runs to completion if the caller disconnects.

Params:

- `path` string: The project's folder

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

Tool (read): Branch, ahead/behind and changed-file counts for several projects at once.

Live on: `git.changed`

Params:

- `paths` [string]: Projects' folders

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
- `content_hash` string (omitted when empty)
- `bytes` base64 (omitted when empty) (nullable)

## `github.clone`

Detached: runs to completion if the caller disconnects.

Params:

- `name_with_owner` string

Result:

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

- `url` string

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
- `agents` [object{Catalog}]
- `github` object{CloneFolder}

## `host.status`

Params:

- none

Result:

- `version` string
- `uptime_seconds` int
- `relay_connected` bool
- `update_failed` string (omitted when empty)
- `battery` object{Battery} (omitted when empty) (nullable)
- `storage` object{Storage} (omitted when empty) (nullable)
- `network` object{Network}
- `capabilities` [string] (omitted when empty) (nullable)

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

- `agent` string
- `credit_id` string

Result:

- `outcome` string

## `mcp.call`

Params:

- `project` string
- `server` string
- `tool` string
- `input` json (omitted when empty) (nullable)
- `read` bool (omitted when empty)
- `confirmed` bool (omitted when empty)

Result:

- `result` json (omitted when empty) (nullable)
- `error` object{Failure} (omitted when empty) (nullable)

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

## `plugins.call`

Params:

- `project` string
- `method` string
- `input` json (omitted when empty) (nullable)
- `confirmed` bool (omitted when empty)

Result:

- `result` json (omitted when empty) (nullable)
- `error` object{Failure} (omitted when empty) (nullable)

## `plugins.subscribe`

Params:

- `project` string
- `method` string
- `input` json (omitted when empty) (nullable)
- `subscription` string

Result:

- `subscription` string
- `revision` int
- `result` json (omitted when empty) (nullable)
- `error` object{Failure} (omitted when empty) (nullable)
- `stream` bool (omitted when empty)

## `plugins.unsubscribe`

Params:

- `subscription` string

Result:

- `ok` bool

## `ports.kill`

Detached: runs to completion if the caller disconnects.

Params:

- `port` int
- `force` bool

Result:

- `processes` int

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

## `projects.delete`

Params:

- `path` string

Result:

- `ok` bool

## `projects.detect_icon`

Params:

- `path` string
- `if_none_match` string

Result:

- `content_hash` string
- `bytes` base64 (omitted when empty) (nullable)
- `not_modified` bool (omitted when empty)
- `remote` string (omitted when empty)

## `projects.list`

Tool (read): The projects on this environment, with each one's folder (the cwd chats take), name, pin and changed files.

Live on: `projects.changed`

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

## `reminders.create`

Tool (write): Notify the user's phones at a set time; a tap opens this chat. Set one only when the user asks for it or agrees to it.

Params:

- `text` string: What the notification says, up to 200 characters
- `in` string (omitted when empty): How long from now, such as 45m, 2h or 3d; or give due_at
- `due_at` int (omitted when empty): When, in Unix milliseconds; or give in
- `chat_id` string (omitted when empty): The chat a tap on the notification opens; this chat when left out
- `path` string (omitted when empty): The project's folder

Result:

- `id` string
- `text` string
- `due_at` int
- `chat_id` string
- `path` string
- `set_by` string
- `created_at` int

## `reminders.delete`

Tool (write): Cancel a pending reminder.

Params:

- `id` string: The reminder's id, as reminders_list gives it

Result:

- `ok` bool

## `reminders.list`

Tool (read): The pending reminders on this environment, soonest first.

Params:

- `path` string (omitted when empty): Only this project's; every reminder when left out

Result:

- `reminders` [object{Reminder}] (nullable)

## `schedules.delete`

Params:

- `id` string

Result:

- `ok` bool

## `schedules.list`

Tool (read): The scheduled prompts on this environment, with each one's project, agent and next run.

Live on: `schedules.changed`

Params:

- none

Result:

- `schedules` [object{Schedule}]

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
- `created_at` int

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
- `ok` bool
- `error` string (omitted when empty)

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
- `ok` bool
- `error` string (omitted when empty)

## `tools.uninstall`

Detached: runs to completion if the caller disconnects.

Params:

- `kind` string

Result:

- `kind` string
- `ok` bool
- `error` string (omitted when empty)

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

Tool (read): The open tunnels: each public URL and the local port it reaches.

Live on: `tunnels.changed`

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

## `usage.history`

Params:

- `since_ms` int
- `until_ms` int

Result:

- `machine_id` string
- `complete` bool
- `homes` [object{Home}]
- `buckets` [object{HourBucket}]
- `sessions` [object{AgentCount}]
- `pricing_fetched` bool
