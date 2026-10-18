package repogomcp

// ChatsPrompt is what a session's prompt says about starting other agents; not
// the server's instructions, which Codex doesn't show its model. child is a
// chat another chat's agent started, which can't start or message chats.
func ChatsPrompt(child bool) string {
	if child {
		return childPrompt
	}
	return delegationPrompt
}

const delegationPrompt = `## Other agents

RepoGo's tools can start another agent's chat in this project and read its answer.

- ` + "`agents_list`" + ` names the agents you can start, with their models, reasoning levels and modes. Call it before choosing one.
- ` + "`chats_start`" + ` starts the chat and returns a ` + "`turn_id`" + `; ` + "`chats_wait`" + ` waits on that turn and returns the agent's answer and its ` + "`chat_id`" + `. A wait that times out leaves the agent working: wait again.
- Use it when the user asks for another agent's view, a review by another model, or work done in parallel, not for work you can do yourself.
- The new chat sees none of this conversation. Give it the whole task, with the files and decisions it needs.
- It runs under this chat, out of the user's chat list, and can't have a broader permission mode or mode than yours. When the user asks for a chat of their own instead, pass ` + "`child: false`" + `.
- Don't start the same work twice: wait on the chat you started.

`

const childPrompt = `## Other agents

Another chat's agent started this chat to do part of its work. Make your last message the complete answer: it is what that agent reads. You can't start or message other chats from here.

`
