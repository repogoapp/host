CREATE TABLE sessions (
  -- An integer surrogate key. Events reference this rather than
  -- (agent, session_id), so the hot table compares integers instead of two
  -- strings per B-tree node, and 42 bytes of repeated key per row disappear.
  sid         INTEGER PRIMARY KEY,
  agent       TEXT    NOT NULL,
  session_id  TEXT    NOT NULL,
  cwd         TEXT    NOT NULL DEFAULT '',
  title       TEXT    NOT NULL DEFAULT '',
  status      TEXT    NOT NULL DEFAULT 'unknown',
  status_at   INTEGER NOT NULL DEFAULT 0,
  last_turn_key TEXT NOT NULL DEFAULT '',
  last_turn_started_at INTEGER,
  last_turn_finished_at INTEGER,
  -- The opening of the agent's newest reply, for the inbox row's preview
  -- line. Derived from the transcript at sync like title.
  last_reply  TEXT    NOT NULL DEFAULT '',
  path        TEXT    NOT NULL DEFAULT '',
  -- When the chat began: the first timestamped transcript line, or the file's
  -- mtime for a transcript with none. Written once and kept, so a resync
  -- cannot move it. Zero is unknown.
  created_at  INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL,
  size_bytes  INTEGER NOT NULL,
  event_count INTEGER NOT NULL,
  -- Fold of the whole event stream. With the stored event_count it answers the
  -- only question generation cares about: was this an append, or a rewrite?
  content_hash INTEGER NOT NULL DEFAULT 0,
  -- Bumped on every wholesale replace so a paging client can detect that
  -- history was rebuilt underneath it.
  generation  INTEGER NOT NULL DEFAULT 0,

  -- The newest request's context fill and window, 0 unknown. Re-derived from the
  -- transcript; a live turn's reading stays on agent.Usage, since the next
  -- sweep would overwrite it here.
  context_used INTEGER NOT NULL DEFAULT 0,
  context_size INTEGER NOT NULL DEFAULT 0,
  -- The newest request's model id, '' unknown. Re-derived from the transcript.
  model       TEXT    NOT NULL DEFAULT '',
  -- What the newest turn ran with (session.Settings), '' or NULL unknown.
  -- Re-derived from the transcript like model.
  permission_mode TEXT NOT NULL DEFAULT '',
  mode            TEXT NOT NULL DEFAULT '',
  reasoning_level TEXT NOT NULL DEFAULT '',
  fast_mode       INTEGER,
  -- With size_bytes this is the freshness check: an append-only file that
  -- changed moved at least one of them, so an unchanged session is skipped
  -- without reparsing.
  synced_at   INTEGER NOT NULL,
  -- Bumped on any write a device would want to hear about; see sync.go.
  rev         INTEGER NOT NULL DEFAULT 0,
  -- Hash of what the chat's chat_search row holds, so a sync that changed
  -- neither title nor text skips rewriting it. See search.go.
  search_hash INTEGER NOT NULL DEFAULT 0,
  -- When the user last sent this chat a prompt, 0 unknown: the transcript's
  -- newest user message, or a turn seen starting live. Only ever moves
  -- forward, so a delayed or partial reading cannot pull it back.
  last_message_at INTEGER NOT NULL DEFAULT 0,
  -- What "Recent activity" sorts on: the last prompt, or the reply ending after
  -- it (updated_at moves on every appended line). A Claude file "finish" is not
  -- a turn end, so it is ignored. Generated so every writer keeps it right.
  activity_at INTEGER GENERATED ALWAYS AS (
    CASE
      WHEN last_turn_finished_at > COALESCE(last_turn_started_at, 0)
       AND NOT (agent = 'claude' AND last_turn_key LIKE 'transcript:%')
      THEN last_turn_finished_at
      ELSE COALESCE(last_turn_started_at, updated_at)
    END) VIRTUAL,
  UNIQUE (agent, session_id)
);

CREATE INDEX idx_sessions_updated ON sessions(updated_at DESC);
CREATE INDEX idx_sessions_rev     ON sessions(rev);
CREATE INDEX idx_sessions_cwd     ON sessions(cwd);
CREATE INDEX idx_sessions_activity ON sessions(activity_at DESC);

CREATE TABLE events (
  sid  INTEGER NOT NULL,
  idx  INTEGER NOT NULL,
  kind TEXT    NOT NULL,
  -- A session is not one linear run. Codex interleaves parallel turns into one
  -- rollout, so grouping by turn is the only way a client can render them apart.
  turn TEXT,
  text TEXT,
  -- Tool calls stay as JSON: the shape is the agent's, it is only ever read
  -- whole, and normalizing it into columns would invent a schema we do not own.
  tool TEXT,
  -- Unix milliseconds from the provider's own line, so a settled turn can be
  -- timed ("Worked for 1m 04s") without the host having watched it run.
  at   INTEGER,
  PRIMARY KEY (sid, idx)
) WITHOUT ROWID;

-- What chats.list's search looks in: one row per chat, rowid = sessions.sid,
-- holding the title and the chat's prompts and replies. See search.go.
-- FTS4 because the driver builds it in; FTS5 needs a build tag on every build.
CREATE VIRTUAL TABLE chat_search USING fts4(title, body, tokenize=porter);

-- Where sync.pull's revisions stand, so a device's cursor outlives a restart
-- (sync.go). One row. The epoch is this file's: a rebuilt cache drops every
-- row's rev, so devices start over. rev_floor is the counter when a row was
-- last deleted, which MAX(sessions.rev) no longer shows.
CREATE TABLE sync_meta (
  epoch     TEXT    NOT NULL,
  rev_floor INTEGER NOT NULL
);

-- A chat deleted from this file, at the revision it went, so sync.pull tells
-- a device by cursor instead of the device sending every id it holds. Kept as
-- long as the file: a rebuild takes a new epoch, and devices start over.
CREATE TABLE deleted_sessions (
  agent      TEXT    NOT NULL,
  session_id TEXT    NOT NULL,
  rev        INTEGER NOT NULL,
  PRIMARY KEY (agent, session_id)
);
CREATE INDEX deleted_sessions_rev ON deleted_sessions (rev);

-- Materialized so project.list is one read, and so what a sweep learned (the
-- repository) and what a watch measured (the diff totals) outlast the call.
CREATE TABLE projects (
  path       TEXT PRIMARY KEY,
  chat_count INTEGER NOT NULL,
  -- When the project was last worked in: its chats' newest activity_at, or
  -- the folder's mtime when it has none. Named as sessions.activity_at is.
  activity_at INTEGER NOT NULL,
  -- The newest last_message_at among the folder's chats, 0 when none has a
  -- prompt. Unlike activity_at it has no folder-mtime fallback and ignores
  -- replies, so it moves only when the user speaks.
  last_message_at INTEGER NOT NULL DEFAULT 0,

  -- The repository this folder checks out, when it is one. Stored because the
  -- list groups by it, and two clients deriving it could disagree.
  repo_owner TEXT NOT NULL DEFAULT '',
  repo_name  TEXT NOT NULL DEFAULT '',

  -- clone | managed | worktree | folder; see projectsync.kindOf.
  kind       TEXT NOT NULL DEFAULT 'folder',
  -- project.detect_icon's content_hash, '' for no icon, so a device fetches an
  -- icon only when this moves.
  icon_hash  TEXT NOT NULL DEFAULT '',
  diff_available INTEGER NOT NULL DEFAULT 0,
  files_changed INTEGER NOT NULL DEFAULT 0,
  additions INTEGER NOT NULL DEFAULT 0,
  deletions INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE INDEX idx_projects_activity ON projects(activity_at DESC);
CREATE INDEX idx_projects_repo ON projects(repo_owner, repo_name);
