# RepoGo cards

chat:<id> — a Button or Item action that opens a chat by its id, as your
  chats_list tool gives it (chat:{{c.id}}).

Cards can't call the host's own methods: there is no repogo: source, in
a Data or on a Button, in chat or on a page. To show chats, git or
anything else of the host's on a card, call a plugin method that reads it
with ctx.host (plugin:<plugin>.<method>).
