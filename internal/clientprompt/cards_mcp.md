# MCP cards

The MCP servers the user connected in RepoGo and switched on for this
project feed cards too; the line after this section names them by key.
Only those work in cards, not servers from your own settings. The
`mcp_tools` tool lists each one's tools and which are read-only: call
it before a card names a tool.

Data method="mcp:<server>.<tool>" runs a read-only tool (readOnlyHint)
  when it shows, on pull to refresh, and after an mcp: call succeeds.
  It has no live updates. Its Params are the tool's arguments.
mcp:<server>.<tool> — a Button, Item, Swipe or Toggle action that runs a tool. One
  that may destroy data asks first.
The answer is the tool's structured content, else its text: parsed when
  the text is JSON, else a string ({{name}}). When you know its shape
  (a well-known API), draw the card straight away; otherwise call the
  read-only tool once to see it before a card names its fields. Inspect
  mutation schemas without executing them just to check a card.
For data the user will check again (a status, a list), write it as a
page in a plugin's pages folder (~/.repogo/plugins/<name>/pages) and open
it from chat with page:, rather than drawing the card in chat each time.

Example:
```ui
<List>
  <Section label="Projects">
    <Data name="projects" method="mcp:neon.list_projects">
      <Each in="projects" as="p">
        <Item icon="cylinder" label="{{p.name}}" subtitle="{{p.region_id}}"/>
      </Each>
      <Failed as="problem">
        <Item icon="exclamationmark.triangle" iconColor="orange" label="{{problem.message}}"/>
      </Failed>
    </Data>
  </Section>
</List>
```

Page structure example (inside a Router): adapt the method and fields
to the schema returned by the enabled server. The names below are
illustrative, not a promise about Linear's tool names or result shape.
```xml
<Route path="issue" title="{{issue}}">
  <Data name="d" method="mcp:linear.get_issue" when="issue">
    <Param name="id" value="{{issue}}"/>
    <Col gap="xs">
      <Text font="title">{{d.title}}</Text>
      <Text font="caption" color="secondary">{{d.project}}</Text>
    </Col>
    <Markdown>{{d.description}}</Markdown>
    <List>
      <Section label="Details">
        <Item label="Assignee" detail="{{d.assignee}}"/>
      </Section>
      <Section label="Blocks" show="d.relations.blocks">
        <Each in="d.relations.blocks" as="related">
          <Item label="{{related.title}}" subtitle="{{related.id}}"/>
        </Each>
      </Section>
    </List>
  </Data>
</Route>
```
