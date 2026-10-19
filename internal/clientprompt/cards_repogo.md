Cards also read and change what the app shows, on this environment,
through the host's own methods: chats in every project, projects, git,
actions, schedules, tunnels and agent limits. Your tools of the same
names (chats_list for chats.list) take the same params: call one first
to see its answer.

Data method="repogo:<method>" runs a read: chats.list (filtered as the
  chat drawer is), chats.info, chats.messages, projects.list,
  git.status, git.changes, git.diff, actions.list, schedules.list,
  tunnels.list, limits.read (each agent's plan and use of its limits).
  It reloads when what it reads changes. Its Params are the method's
  params.
repogo:<method> — a Button, Item or Swipe action that runs one:
  chats.update (rename, title), chats.pin, chats.resolve, chats.send
  (turn={"prompt":…}), chats.start, chats.stop, actions.run,
  actions.start, actions.stop.
chat:<id> — opens a chat by the id chats.list gives (chat:{{c.id}}).

Example:
```ui
<List>
  <Section label="Open chats">
    <Data name="open" method="repogo:chats.list">
      <Param name="resolved" value="false" type="boolean"/>
      <Param name="limit" value="20" type="number"/>
      <Each in="open.chats" as="c">
        <Item icon="bubble.left" label="{{c.title}}" subtitle="{{c.activity_at | relative}}" action="chat:{{c.id}}">
          <Swipe label="Resolve" icon="checkmark" action="repogo:chats.resolve">
            <Param name="chat_id" value="{{c.id}}"/>
            <Param name="resolved" value="true" type="boolean"/>
          </Swipe>
        </Item>
      </Each>
    </Data>
  </Section>
</List>
```
