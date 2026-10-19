The MCP servers the user connected in RepoGo and switched on for this
project feed cards too; the line after this section names them by key.
Only those work in cards, not servers from your own settings. The
`mcp_tools` tool lists each one's tools and which are read-only: call
it before a card names a tool.

Data method="mcp:<server>.<tool>" runs a read-only tool (readOnlyHint)
  when it shows, on pull to refresh, and after an mcp: call succeeds.
  It has no live updates. Its Params are the tool's arguments.
mcp:<server>.<tool> — a Button or Item action that runs any tool. One
  that may destroy data asks first.
The answer is the tool's structured content, else its text: parsed when
  the text is JSON, else a string ({{name}}). When you know its shape
  (a well-known API), draw the card straight away; otherwise call the
  tool yourself once to see it before a card names its fields.
For data the user will check again (a status, a list), write it as a
page in .repogo/tabs and open it from chat with page:, rather than
drawing the card in chat each time.

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
