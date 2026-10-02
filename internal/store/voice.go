package store

import (
	"database/sql"
	"math/rand/v2"
	"strings"
)

// VoiceHandle is a chat's spoken name: what the voice agent calls it ("Blue
// Otter, run the tests") and how a spoken name resolves back to the chat.
type VoiceHandle struct {
	Prefix  string `json:"prefix"`
	Name    string `json:"name"`
	Display string `json:"display"`
	Key     string `json:"key"`
}

func newVoiceHandle(prefix, name string) *VoiceHandle {
	return &VoiceHandle{Prefix: prefix, Name: name, Display: prefix + " " + name, Key: voiceKey(prefix, name)}
}

func voiceKey(prefix, name string) string { return strings.ToLower(prefix + "-" + name) }

// voiceHandleFrom reads the nullable pair a LEFT JOIN on voice_handles scans into.
func voiceHandleFrom(prefix, name sql.NullString) *VoiceHandle {
	if !prefix.Valid || !name.Valid {
		return nil
	}
	return newVoiceHandle(prefix.String, name.String)
}

var voicePrefixes = []string{
	"Blue", "Gold", "Silver", "Green", "Bright", "Swift", "Quiet", "Bold", "Lucky", "North",
	"Sunny", "Clear", "Fresh", "Rapid", "Wild", "Royal", "Soft", "Deep", "Amber", "Coral",
	"Cobalt", "Crimson", "Ivory", "Jade", "Onyx", "Scarlet", "Teal", "Violet", "Bronze", "Frost",
}

var voiceNames = []string{
	"Otter", "Jasper", "Echo", "Atlas", "Rocket", "Pixel", "Cosmo", "Pepper", "Moose", "Cedar",
	"Ranger", "Falcon", "Orbit", "Ruby", "Comet", "River", "Scout", "Panda", "Maple", "Tango",
	"Nova", "Copper", "Harbor", "Summit", "Boulder", "Nimbus", "Sparrow", "Clover", "Maverick", "Canyon",
	"Willow", "Aspen", "Birch", "Robin", "Heron", "Marlin", "Bison", "Cobra", "Lynx", "Puma",
	"Raven", "Wren", "Finch", "Delta", "Mesa",
}

// voicePoolSize caps how many chats hold a handle at once. Past it the newest
// chats take the handles of the stalest, so only chats nobody has touched in a
// long while go unnamed; nobody speaks to 1,350 chats.
var voicePoolSize = len(voicePrefixes) * len(voiceNames)

// assignVoiceHandles names unnamed chats newest first while the pool lasts, then
// recycles the stalest holder's handle for a newer chat, so a name keeps meaning
// one chat. Both sides bump rev, and the holders that lost a name are returned.
func (s *Store) assignVoiceHandles(tx *sql.Tx) (touched []ChatID, err error) {
	type chat struct {
		agent, sessionID string
		updatedAt        int64
		prefix, name     string
	}
	scan := func(query string, args []any, withHandle bool) ([]chat, error) {
		rows, err := tx.Query(query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []chat
		for rows.Next() {
			var c chat
			dest := []any{&c.agent, &c.sessionID, &c.updatedAt}
			if withHandle {
				dest = append(dest, &c.prefix, &c.name)
			}
			if err := rows.Scan(dest...); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}

	unnamed, err := scan(`SELECT s.agent, s.session_id, s.updated_at FROM sessions s
		WHERE NOT EXISTS (SELECT 1 FROM state.voice_handles v WHERE v.agent = s.agent AND v.session_id = s.session_id)
		ORDER BY s.updated_at DESC LIMIT ?`, []any{voicePoolSize}, false)
	if err != nil || len(unnamed) == 0 {
		return nil, err
	}
	// Every handle is taken, its chat cached or not: one not yet re-imported
	// after a rebuild still answers to its name.
	taken, err := takenVoiceKeys(tx)
	if err != nil {
		return nil, err
	}
	// Only a cached holder can be recycled, since its age is known.
	held, err := scan(`SELECT v.agent, v.session_id, s.updated_at, v.prefix, v.name FROM state.voice_handles v
		JOIN sessions s ON s.agent = v.agent AND s.session_id = v.session_id
		ORDER BY s.updated_at ASC`, nil, true)
	if err != nil {
		return nil, err
	}
	var free [][2]string
	for _, p := range voicePrefixes {
		for _, n := range voiceNames {
			if !taken[voiceKey(p, n)] {
				free = append(free, [2]string{p, n})
			}
		}
	}

	for _, c := range unnamed {
		var prefix, name string
		switch {
		case len(free) > 0:
			i := rand.IntN(len(free))
			prefix, name = free[i][0], free[i][1]
			free[i] = free[len(free)-1]
			free = free[:len(free)-1]
		case len(held) > 0 && held[0].updatedAt < c.updatedAt:
			stale := held[0]
			held = held[1:]
			if _, err := tx.Exec(`DELETE FROM state.voice_handles WHERE agent = ? AND session_id = ?`, stale.agent, stale.sessionID); err != nil {
				return nil, err
			}
			if err := s.bumpRev(tx, stale.agent, stale.sessionID); err != nil {
				return nil, err
			}
			touched = append(touched, chatID(stale.agent, stale.sessionID))
			prefix, name = stale.prefix, stale.name
		default:
			// Everything left is older than every holder.
			return touched, nil
		}
		if _, err := tx.Exec(`INSERT INTO state.voice_handles (agent, session_id, prefix, name, key) VALUES (?, ?, ?, ?, ?)`,
			c.agent, c.sessionID, prefix, name, voiceKey(prefix, name)); err != nil {
			return nil, err
		}
		if err := s.bumpRev(tx, c.agent, c.sessionID); err != nil {
			return nil, err
		}
	}
	return touched, nil
}

// takenVoiceKeys is the key of every handle in state.voice_handles.
func takenVoiceKeys(tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT key FROM state.voice_handles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	taken := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		taken[key] = true
	}
	return taken, rows.Err()
}
