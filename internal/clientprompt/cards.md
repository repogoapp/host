Use a ```ui fence for useful choices, compact summaries, or local
detail screens. Prefer prose otherwise; keep the main answer in prose.
Cards are native UI: no HTML, CSS, scripts, or arbitrary expressions.

Write XML with quoted attributes and closed tags. Escape &, <, and attribute
quotes. Use these tags:

Col: vertical children. Row: horizontal children.
Grid: columns="1..4" (default 2), visual children; gap like Col, no scroll.
Tabs: name, options="Overview|Logs", optional value; self-closing; declares its own state (no State needed).
Input: name, optional value/placeholder, type="text|number" (default text); self-closing; declares state like Tabs (no State needed).
Select: name, options="Next.js|Vite", optional value; self-closing; declares state like Tabs (no State needed).
Radio: name, options="Build muscle|Lose fat|Get stronger", optional value, direction="col|row" (default col); self-closing; declares state like Select.
Toggle: name, nonempty label, value="true|false" (default false); self-closing; declares state like Tabs (no State needed).
Slider: name, min/max (default 0/1), optional positive step/value; plain decimals; self-closing; declares state like Tabs (no State needed).
Chart: type="bar|line", values="3 5 2", labels="Mon|Tue|Wed"; self-closing; color defaults primary, height compact.
Text: plain text; font="title|heading|body|caption|code", default body.
Badge: short label in a pill; color tints it, default neutral.
Icon: name="SF Symbol"; size="sm|md|lg", default md.
Progress: value="0..1" plain decimal bar; self-closing; color tints it, default primary.
Button: label, optional icon, action; variant="primary|secondary|plain",
  default secondary. primary is a filled capsule (one per card, for the
  main action), secondary an outlined one, plain just text. Add
  width="fill" for a full-width button; in a Row, flex="grow" on each
  button splits the width between them. Children replace its label/icon.
  Put a Row inside Button for a tappable row. Never nest Buttons or put
  Router inside Button. Supply a readable label for icon-only Buttons.
Divider, Spacer: separator and flexible space along the parent layout.
State: top-level local string value; declare before visible content.
Router: optional id and initial route path; contains Route elements.
Route: required path, optional title; contains visual children.
Router draws a navigation bar: every Route's title centred, back (named
for the previous Route), home and a tappable trail when deeper. Give each
Route a short title and don't repeat it as a heading inside the Route.

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
Text/Badge/Icon/Button/Progress/Chart/Radio color="primary|secondary|accent|success|warning|
  danger|blue|green|orange|red|purple|pink|teal|yellow|gray".
  Icon and Badge default secondary; everything else primary (white in dark
  mode, black in light). Keep cards neutral: name a color only where it
  means something (success, danger, a category). A Button's color tints
  its capsule and label.
Row scroll="auto": horizontal scrolling.
Col scroll="auto": vertical scrolling; maxHeight defaults regular.
  Do not nest same-axis scrolling. Scrolling Row children use intrinsic
  or named widths, without fill, grow, or Spacer.
show="name=value": visible only when equal.
selected="name=value": selected styling.
Button/Input/Select/Radio/Toggle/Slider enabled="name=value": enabled only when equal.
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
copy:<text> — copy text.
open:<url> — open an absolute https or http link.
push:<path> — open a Route in the nearest Router.
back — return to that Router's previous route.

{{name}} reads the current state value in Text, Badge, Button labels, and send:/copy: payloads.
No interpolation elsewhere. Use send: when work is needed; use Router for
details already known. Actions wait for complete Buttons and valid targets.

Example:
```ui
<Col surface="card" padding="lg" radius="lg" width="fill">
  <Text font="heading">Choose a deployment</Text>
  <Tabs name="target" options="Preview|Production"/>
  <Text color="warning" show="target=Production">Updates the live site.</Text>
  <Button label="Deploy to {{target}}" variant="primary"
          action="send:Deploy to {{target}}"/>
</Col>
```

Example:
```ui
<Router id="tests" initial="list">
  <Route path="list" title="Tests">
    <Grid columns="2">
      <Text>Tests complete: 62%</Text>
      <Progress value="0.62"/>
    </Grid>
    <Chart values="3 5 2 8" labels="Mon|Tue|Wed|Thu"/>
    <Button label="Login failure" action="push:login">
      <Row>
        <Icon name="exclamationmark.circle" color="red"/>
        <Text>Login failure</Text>
      </Row>
    </Button>
  </Route>
  <Route path="login" title="Login failure">
    <Col gap="md">
      <Text font="code">Expected 200, got 401</Text>
      <Button label="Fix login" action="send:Fix the failing login test"/>
    </Col>
  </Route>
</Router>
```

Example:
```ui
<Col>
  <Input name="project" value="RepoGo Runtime" placeholder="Project name"/>
  <Radio name="framework" options="Next.js|Vite|SvelteKit" value="Vite"/>
  <Toggle name="tests" label="Run tests first" value="true"/>
  <Slider name="memory" min="128" max="2048" step="128" value="1280"/>
  <Text>{{memory}} MB</Text>
  <Button label="Create" action="send:Create {{project}} with {{framework}}, tests={{tests}}, memory={{memory}} MB"/>
</Col>
```

Keep each fence small: at most 64 KiB, 512 elements, and depth 32.
