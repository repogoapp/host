// The SQL that carries merge rules of its own. Short statements stay beside
// the code that runs them.
//
// A turn writes its chat's sessions row in this order, and each write is one
// chats.changed to every device:
//
//  1. statusUpsert, as the turn starts (SetStatus): status, turn timing and
//     last_message_at, plus the title (the device's, else the prompt's opening
//     words) and the model the device asked for when the row has neither yet,
//     and the permission the turn runs with. A new chat's card therefore shows
//     them from the first push.
//  2. sessionUpsert, once the agent has written the prompt to its transcript
//     (SyncBatch): the events rows, and the title when the agent's files name
//     one or the row has none.
//  3. sessionUpsert, once the reply is in the transcript: last_reply, context,
//     and the model the provider actually ran, which replaces the requested one.
//  4. statusUpsert, as the turn ends: status and the finish time.
//
// Steps 2 and 3 can land as one sweep when the file changes quickly.
package store

// Writes 2 and 3 above. The generation bump is a parameter: it means "the
// history you hold is gone", so an append, which a reader keeps, must not bump it.
const sessionUpsert = `
INSERT INTO sessions (agent, session_id, cwd, title, last_reply, path, created_at, updated_at, size_bytes, event_count, content_hash, generation, synced_at, context_used, context_size, model, permission_mode, mode, reasoning_level, fast_mode, last_message_at, rev)
VALUES (?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(agent, session_id) DO UPDATE SET
  cwd = excluded.cwd, last_reply = excluded.last_reply, path = excluded.path,
  -- The agent's own title (a rename, its generated one) always wins. Opening
  -- words only fill an empty row, so the title a device started the chat with stays.
  title = CASE WHEN ? OR sessions.title = '' THEN excluded.title ELSE sessions.title END,
  -- A chat begins once. The first value written stays, whether it came from
  -- the transcript or from the hook that saw the prompt go out.
  created_at = CASE WHEN sessions.created_at > 0 THEN sessions.created_at ELSE excluded.created_at END,
  updated_at = excluded.updated_at, size_bytes = excluded.size_bytes,
  event_count = excluded.event_count, content_hash = excluded.content_hash,
  synced_at = excluded.synced_at,
  -- A reparse with no reading keeps the last one: providers write it only after
  -- a request, so its absence is a truncated segment, not an empty window.
  context_used = CASE WHEN excluded.context_used > 0 THEN excluded.context_used ELSE sessions.context_used END,
  context_size = CASE WHEN excluded.context_size > 0 THEN excluded.context_size ELSE sessions.context_size END,
  model = CASE WHEN excluded.model != '' THEN excluded.model ELSE sessions.model END,
  permission_mode = CASE WHEN excluded.permission_mode != '' THEN excluded.permission_mode ELSE sessions.permission_mode END,
  mode = CASE WHEN excluded.mode != '' THEN excluded.mode ELSE sessions.mode END,
  reasoning_level = CASE WHEN excluded.reasoning_level != '' THEN excluded.reasoning_level ELSE sessions.reasoning_level END,
  fast_mode = COALESCE(excluded.fast_mode, sessions.fast_mode),
  last_message_at = MAX(sessions.last_message_at, excluded.last_message_at),
  generation = sessions.generation + ?,
  rev = excluded.rev
RETURNING sid, title, search_hash`

// Writes 1 and 4 above. An observation can precede the transcript: size_bytes
// -1 never matches a file, so the next sweep parses it. A newer status_at wins;
// an older one is a delayed report and must not roll the row back.
const statusUpsert = `
INSERT INTO sessions (agent, session_id, cwd, title, model, permission_mode, created_at, updated_at, size_bytes,
  event_count, synced_at, status, status_at, last_turn_key, last_turn_started_at, last_turn_finished_at, last_message_at, rev)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, -1, 0, 0, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent, session_id) DO UPDATE SET
  status = CASE WHEN excluded.status_at >= sessions.status_at THEN excluded.status ELSE sessions.status END,
  status_at = MAX(sessions.status_at, excluded.status_at),
  cwd = CASE WHEN sessions.cwd = '' THEN excluded.cwd ELSE sessions.cwd END,
  -- Only an empty row takes them: once the transcript names a title or model,
  -- a request must not overwrite it, or the next sync would switch it back.
  title = CASE WHEN sessions.title = '' THEN excluded.title ELSE sessions.title END,
  model = CASE WHEN sessions.model = '' THEN excluded.model ELSE sessions.model END,
  -- A hook or a turn's end names no permission and keeps the row's.
  permission_mode = CASE WHEN excluded.permission_mode != '' THEN excluded.permission_mode ELSE sessions.permission_mode END,
  last_turn_key = excluded.last_turn_key,
  last_turn_started_at = excluded.last_turn_started_at,
  last_turn_finished_at = excluded.last_turn_finished_at,
  last_message_at = MAX(sessions.last_message_at, excluded.last_message_at),
  rev = excluded.rev`
