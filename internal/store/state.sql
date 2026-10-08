-- state.db: what the user made or chose, which no transcript holds. Never
-- deleted. Runs on each open; new data is a new table or a new nullable
-- column, nothing is renamed or retyped, and the open checks every column
-- (state.go). Plan 374.

-- What the user set on a chat. NULL is not set; an all-NULL row is deleted.
-- One writer: updateChatMarks (marks.go).
CREATE TABLE IF NOT EXISTS chat_marks (
  agent       TEXT NOT NULL,
  session_id  TEXT NOT NULL,
  resolved_at INTEGER,
  PRIMARY KEY (agent, session_id)
);

-- What the user set on a project. NULL is not set; an all-NULL row is deleted.
-- One writer: updateProjectMarks (projectmarks.go).
CREATE TABLE IF NOT EXISTS project_marks (
  path      TEXT PRIMARY KEY,
  picked_at INTEGER,  -- picked or made in the folder picker
  pinned_at INTEGER,  -- pinned to the top of the list
  name      TEXT      -- the user's name for it; never ''
) WITHOUT ROWID;

-- Each chat's spoken name for the voice agent: a name the user has heard
-- must survive a rebuild. key is unique so no two chats answer to one name.
-- Minted by the import (voice.go).
CREATE TABLE IF NOT EXISTS voice_handles (
  agent      TEXT NOT NULL,
  session_id TEXT NOT NULL,
  prefix     TEXT NOT NULL,
  name       TEXT NOT NULL,
  key        TEXT NOT NULL UNIQUE,
  PRIMARY KEY (agent, session_id)
);

-- Turns waiting behind a chat's running turn, so a restart keeps them. Keyed
-- by the Manager's chat id: a new chat's queue exists under a temporary id
-- before the agent names its session. One writer: SaveQueue (queue.go).
CREATE TABLE IF NOT EXISTS queued_turns (
  turn_id   TEXT    PRIMARY KEY,
  chat_id   TEXT    NOT NULL,
  position  INTEGER NOT NULL,
  queued_at INTEGER NOT NULL,
  -- Restored after a restart: waits for Send Now instead of running.
  held      INTEGER NOT NULL DEFAULT 0,
  -- agent.TurnRequest as JSON: the prompt, its settings and attachment paths.
  request   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_queued_turns_chat ON queued_turns(chat_id, position);

-- Moves on every change to a chat's queue and is kept when the queue empties,
-- so a device refetches the turns only when they changed.
CREATE TABLE IF NOT EXISTS chat_queues (
  chat_id TEXT    PRIMARY KEY,
  rev     INTEGER NOT NULL
) WITHOUT ROWID;

-- A prompt this host runs on its own at a set time, each run a new chat.
-- One writer: the schedule methods in schedules.go.
CREATE TABLE IF NOT EXISTS schedules (
  id          TEXT    PRIMARY KEY,
  title       TEXT    NOT NULL,
  prompt      TEXT    NOT NULL,
  path        TEXT    NOT NULL,  -- the project
  agent       TEXT    NOT NULL,
  config      TEXT    NOT NULL,  -- agent.TurnConfig as JSON
  frequency   TEXT    NOT NULL,  -- hourly | daily | weekly
  hour        INTEGER NOT NULL,
  minute      INTEGER NOT NULL,
  weekday     INTEGER NOT NULL,  -- 1..7, Sunday = 1
  timezone    TEXT    NOT NULL,  -- IANA
  enabled     INTEGER NOT NULL,
  -- Nothing due at or before the last save runs, so an edit never fires a
  -- time already passed.
  saved_at    INTEGER NOT NULL,
  -- The last due time taken. A due time is taken once, so a restart or a
  -- second tick never runs it twice.
  last_due_at INTEGER
) WITHOUT ROWID;
