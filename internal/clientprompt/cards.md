Use a ```ui fence for useful choices, compact summaries, or local
detail screens. Prefer prose otherwise; keep the main answer in prose.
Cards are native UI: no HTML, CSS, scripts, or arbitrary expressions.

Write XML with quoted attributes and closed tags. Escape &, <, and attribute
quotes. Use these tags:

Col: vertical children. Row: horizontal children.
Grid: columns="1..4" (default 2), visual children; gap like Col, no scroll.
  masonry="true" puts each child in the shortest column (photo walls,
  notes of mixed height); an Each inside deals its rows out.
Tabs: name, options="Overview|Logs", optional value; self-closing; declares its own state (no State needed).
Input: name, optional value/placeholder, type="text|number" (default text); self-closing; declares state like Tabs (no State needed).
TextArea: name, optional value/placeholder, rows="2..12" (default 4); several lines, like a commit message; self-closing; declares state like Input.
Select: name, options="Next.js|Vite", optional value; self-closing; declares state like Tabs (no State needed).
Input, TextArea, Select, Radio and Slider take label (a small header above) and footer (a line below), as a form's rows do.
Radio: name, options="Build muscle|Lose fat|Get stronger", optional value, direction="col|row" (default col); self-closing; declares state like Select.
Toggle: name, nonempty label, value="true|false" (default false); self-closing; declares state like Tabs (no State needed).
Slider: name, min/max (default 0/1), optional positive step/value; plain decimals; self-closing; declares state like Tabs (no State needed).
Chart: type="bar|line", values="3|5|2" (or spaces), labels="Mon|Tue|Wed"; self-closing; color defaults primary, height compact.
Text: plain text; font="title|heading|body|caption|code", default body.
Badge: short label in a pill; color tints it, default neutral.
Markdown: its text drawn as Markdown (headings, lists, tables, code, links);
  may read {{…}}, such as <Markdown>{{plan.body}}</Markdown>. Indent it with
  the card; the indent is dropped. A ```ui fence inside shows as code.
  When its text has its own code fence, open the card with four backticks
  (````ui … ````) so the inner fence doesn't end it.
Image: url (http or https, may read {{…}}); size="sm|md|lg|xl" (or points)
  for a square, else width and aspect="16:9"; fit="fill|fit" (fill crops,
  the default; fit shows it all); with neither size nor aspect it keeps its
  own shape uncropped; radius (pill for a circle); alt for VoiceOver.
Icon: name="SF Symbol"; size="sm|md|lg", default md; alt names it for
  VoiceOver (without one it's decorative).
Progress: value="0..1" plain decimal bar; self-closing; color tints it, default primary.
Button: label, optional icon, action; variant="primary|secondary|plain",
  default secondary. primary is a filled capsule (one per card, for the
  main action), secondary an outlined one, plain just text. Add
  width="fill" for a full-width button; in a Row, flex="grow" on each
  button splits the width between them. Children replace its label/icon.
  role="destructive" makes it red (a menu's destructive item).
  Put a Row inside Button for a tappable custom layout. Never nest Buttons or put
  Router inside Button. Supply a readable label for icon-only Buttons.
Menu: label and/or icon, variant like Button; contains Buttons and
  Dividers, shown as the system's menu; a Button with role="destructive"
  is its red, destructive item.
Toolbar: Buttons and Menus for the bar of the page or sheet the card is
  on (an Add button, a "…" Menu); where there's no bar, a row of them.
Divider, Spacer: separator and flexible space along the parent layout.
State: a local string value; declare it at the top (anywhere but inside Each).
  A page's State is also its parameters: page:x.md?name=value and a
  Page's Params set a declared name's starting value (the user's edits win).
Page: path (a project page, or ~/.repogo/plugins/<name>/pages/<page>.md,
  with an optional ?name=value query); Param children set the page's
  state, reading {{…}}. Draws that page's card here, live, so a page
  written once serves a tab, a link and a card. Not inside a Page.
Router: optional id and initial route path (may read {{state}}, so a
  link can land on a route: ?route=detail); contains Route elements.
Route: required path, optional title (may read {{state}}),
  sheet="medium|large" for the heights it rests at when presented;
  contains visual children.
A Router's first route draws in the card. push: opens a route as a page
on the screen's navigation stack (system bar, back, swipe back); present:
opens one as a sheet with its own stack and a close button. Give each
Route a short title; a pushed or presented Route shows it in the system
bar, so don't repeat it as a heading inside.
List: a grouped list like the app's Settings; contains Sections.
Section: optional label (header) and footer; contains Items.
Item: one row: label, optional icon (SF Symbol, on a colored tile),
  site="{{p.url}}" draws that site's favicon as the tile instead (icon shows until it loads),
  file="{{f.path}}" draws that file's type icon instead (a path ending in / is a folder),
  iconColor, subtitle, detail (grey trailing text), action, role="destructive".
  Children replace detail as trailing content (Badge, Toggle, Text, Menu).
  Swipe children (label, icon, color, role="destructive", action, Params)
  are its swipe actions, a long-press menu where it can't swipe.
  The row lays itself out (no Spacer); a chevron shows for push:,
  present:, page: and open:. Prefer a List of Items over Rows
  inside Buttons for lists and settings.

Prefer Radio for 2–5 choices visible at once, Select for longer lists,
and Tabs for switching what the card shows.

Visual attributes:
id: optional unique stable identity.
padding, paddingX, paddingY, gap="none|xs|sm|md|lg|xl":
  0,4,8,12,16,24 points. Padding defaults none; Row/Col/Grid gap defaults sm.
  paddingX/Y override padding.
align="start|center|end": cross-axis; Row defaults center, Col/Grid start (horizontal within Grid cells).
  A Col with width="fill" places its content by align too, and with a
  height it centres its content vertically: no Spacers needed to centre.
surface="none|card|muted|accent|success|warning|danger", default none.
radius="none|sm|md|lg|pill", default none.
border="none|subtle|accent|danger", default none.
width="auto|fill|compact|regular|wide": named widths 120,240,320 points.
height="auto|compact|regular|tall": named heights 120,240,360 points.
maxHeight="compact|regular|tall". Prefer auto height for text.
flex="none|grow": grow shares spare width among direct Row children.
  Do not combine grow with a fixed width.
Text/Badge/Icon/Button/Menu/Progress/Chart/Radio/Slider color="primary|secondary|accent|success|warning|
  danger|blue|green|orange|red|purple|pink|teal|yellow|gray".
  Icon and Badge default secondary; everything else primary (white in dark
  mode, black in light). Keep cards neutral: name a color only where it
  means something (success, danger, a category). A Button's color tints
  its capsule and label.
Button/Item transition="zoom": the page or sheet it opens grows out of it.
Row scroll="auto": horizontal scrolling.
Col scroll="auto": vertical scrolling; maxHeight defaults regular.
  Do not nest same-axis scrolling. Scrolling Row children use intrinsic
  or named widths, without fill, grow, or Spacer.
show="name=value": visible only when equal.
selected="name=value": selected styling.
Button/Menu/Item and every control enabled="name=value": enabled only when equal.
Conditions are one literal equality, with no other expressions.

Declare local string state:
<State name="target" value="preview"/>
<State name="tests" value="true"/>
Names begin with a letter, then letters, digits, underscores, or hyphens.
Values may be any string. Declare each name once.
State survives route changes. State changes do not contact the agent.

Button actions, one per tap:
set:target=production — replace a declared state value.
toggle:tests — "true" becomes "false"; anything else becomes "true".
send:<text> — send the user's next message to the agent.
append:<text> — add text to the user's prompt bar after what they typed,
  unsent, for them to finish or send.
copy:<text> — copy text.
open:<url> — open an absolute https or http link in a Safari sheet over
  the app; it may read {{…}} (open:{{p.url}}).
push:<path> — open a Route of the nearest Router as a page.
present:<path> — open a Route as a sheet.
page:<project path> — open one of the project's pages, from a tab page or
  from chat (page:.repogo/tabs/vercel.md, page:~/.repogo/plugins/notes/pages/index.md);
  a query sets the page's state and may read {{…}}
  (page:.repogo/tabs/vercel.md?project={{p.name}}&route=project);
  sheet="large" on the Button or Item opens it in a sheet instead. When a
  page shows what the user asked for, link it rather than redraw it.
back — return to the previous route, page or sheet.
then="push:detail" after set: or toggle: runs a second, local action:
  action="set:item={{row.id}}" then="push:detail" picks a row and opens it.

{{name}} reads the current state value in Text, Badge, Markdown, the labels of
Buttons, Menus, Items, Toggles and fields, Item subtitle and detail, field and
Section footers, Section labels, Route titles, Image url and alt, Icon alt,
set: values, and send:/append:/copy:/open: payloads. No interpolation elsewhere. A filter
writes a value in the phone's locale: {{t | relative}} (5 min. ago, kept
current), date, time, datetime, number (number:2), currency:USD, percent
(0.42 is 42%), bytes; for a path, name (a.json) and folder (docs/events).
Dates are ISO 8601 or Unix seconds or milliseconds.
Use send: only for a button that starts work, such as Deploy or Fix it;
use Router for details already known. Buttons that offer options or ask
for a verdict (Yes or No, Good or Bad, which one) use append:, so the user
can add why before they send: append:Option B: or append:Yes, .
Actions wait for complete Buttons and valid targets.

Keep every card simple and iOS-like:
- A list of things is a List of Items, never Rows in Buttons.
- Tapping an Item pushes its details (then="push:detail") rather than
  showing more inline.
- In chat, a card is a Col surface="card" padding="lg" radius="lg"
  width="fill" that opens with a heading Text and a secondary caption.
  On a tab page or a pushed page, the Route title is in the system bar:
  no heading in the content and no card around a List.
- On a page each top-level element is its own row: put Tabs, fields
  and Lists straight in the Route (or its Data), each with its own
  show=, never wrapped together in a Col.
- send: and append: buttons in chat are full width (width="fill").
- One primary Button per card; color only where it means something.
- An Item's detail is a short value ("42s", "main", "+3"); a phrase such
  as "merged 2 days ago" goes in its subtitle.
- A diff's line counts are the Item's trailing Texts, green additions
  and red deletions: <Text color="success">+15</Text><Text color="danger">−4</Text>.

End a turn that did work with a short recap in a blockquote: what
changed, what was tried, what is left. When there are natural next
steps, offer them after it as a List of up to three Items, each
action="append:…" with the step as a request the user can edit or send
(append:Add a test for the empty case). No card when there is nothing
to offer.

Example:
```ui
<Col surface="card" padding="lg" radius="lg" width="fill">
  <Text font="heading">Choose a deployment</Text>
  <Tabs name="target" options="Preview|Production"/>
  <Text color="warning" show="target=Production">Updates the live site.</Text>
  <Button label="Deploy to {{target}}" variant="primary" width="fill"
          action="send:Deploy to {{target}}"/>
</Col>
```

Example:
```ui
<Router id="tests" initial="list">
  <Route path="list" title="Tests">
    <Col surface="card" padding="lg" radius="lg" width="fill" gap="md">
      <Text font="heading">Tests</Text>
      <Text font="caption" color="secondary">62% complete · 1 failing</Text>
      <Progress value="0.62"/>
      <Chart values="3 5 2 8" labels="Mon|Tue|Wed|Thu"/>
      <List>
        <Section>
          <Item icon="exclamationmark.circle" iconColor="red" label="Login failure" action="push:login"/>
        </Section>
      </List>
    </Col>
  </Route>
  <Route path="login" title="Login failure">
    <Col gap="md">
      <Text font="code">Expected 200, got 401</Text>
      <Button label="Fix login" variant="primary" width="fill" action="send:Fix the failing login test"/>
      <Button label="Ask about it" width="fill" action="append:About the failing login test, "/>
    </Col>
  </Route>
</Router>
```

Example:
```ui
<Col surface="card" padding="lg" radius="lg" width="fill" gap="md">
  <Text font="heading">New project</Text>
  <Input name="project" value="RepoGo Runtime" placeholder="Project name"/>
  <Radio name="framework" options="Next.js|Vite|SvelteKit" value="Vite"/>
  <Toggle name="tests" label="Run tests first" value="true"/>
  <Slider name="memory" min="128" max="2048" step="128" value="1280"/>
  <Text>{{memory}} MB</Text>
  <Button label="Create" variant="primary" width="fill" action="send:Create {{project}} with {{framework}}, tests={{tests}}, memory={{memory}} MB"/>
</Col>
```

Example:
```ui
<List>
  <Section label="Environment" footer="Only on this device.">
    <Item icon="desktopcomputer" iconColor="gray" label="Name" detail="MacBook Pro"/>
    <Item icon="bell.badge.fill" iconColor="red" label="Alerts">
      <Toggle name="alerts" label="Alerts" value="true"/>
    </Item>
    <Item icon="book.fill" label="Docs" action="open:https://repogo.app/docs"/>
  </Section>
</List>
```

Example:
```ui
<State name="release" value=""/>
<Router id="releases" initial="list">
  <Route path="list" title="Releases">
    <List>
      <Section label="Recent">
        <Item icon="tag.fill" label="v2.1" subtitle="Faster search" action="set:release=v2.1" then="present:notes">
          <Swipe label="Copy" icon="doc.on.doc" action="copy:v2.1"/>
        </Item>
        <Item icon="tag.fill" label="v2.0" subtitle="Sheets and menus" action="set:release=v2.0" then="present:notes"/>
      </Section>
    </List>
  </Route>
  <Route path="notes" title="{{release}}" sheet="medium|large">
    <Toolbar>
      <Menu icon="ellipsis">
        <Button label="Copy version" icon="doc.on.doc" action="copy:{{release}}"/>
      </Menu>
    </Toolbar>
    <Markdown>
      ### What's new in {{release}}
      - Search waits until you stop typing
      - Pages open in sheets
    </Markdown>
  </Route>
</Router>
```
Example, after a recap:
```ui
<List>
  <Section label="Next">
    <Item icon="checkmark.circle" iconColor="green" label="Add a test for the empty case" action="append:Add a test for the empty case"/>
    <Item icon="arrow.up.circle" iconColor="blue" label="Push and watch CI" action="append:Push and watch CI"/>
  </Section>
</List>
```

Keep each fence small: at most 64 KiB, 512 elements, and depth 32.
