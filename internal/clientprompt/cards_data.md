Cards read and write live data, from the sources below:

Data: name, method="<source>:<name>" (a query or a stream). Children read
  the answer as {{name}}, and its fields as {{name.field}} (objects to any
  depth; an array gives {{name.count}}, its elements only through
  Each: no {{name.0}}). Optional debug="true" prints the
  answer; when="stateName" waits until that state is non-empty (any text,
  "false" and "0" too); debounce="300" waits that many ms after its
  params change, for a search Input.
  A stream's answer is the list of its items so far, growing live under
  a Live badge: an Each over it scrolls like a terminal, stuck to the
  newest; keep="300" shows that many newest items (default 500).
Param: name, value, optional type="string|number|boolean|json|null";
  the first children of a Data, or of a Button or Item that calls a
  source, one per input field. A value reads state and the rows and
  answers of the Data and Each around it, never its own Data's answer.
  Without type, a value that is exactly {{path}} into data sends that
  JSON as is (a number, an array); anything else is text. In a
  type="json" value, a {{name}} inside quotes is escaped, so typed text
  may hold quotes and line breaks: {"prompt": "{{message}}"}. A Param that
  can't be read shows its error in Failed, or under the button.
Each: in="<binding path>", as="row", optional key (default id); repeats
  its children per element, reading {{row.field}}. No Tabs, Input,
  TextArea, Select, Radio, Toggle, Slider or Router inside Each.
Chart in="<binding path>" value="field" label="field": a bar or point
  per element; value and label are paths into one (totals.spent), or
  label is text reading it by as="row" ({{row.date | date}}). Several
  lines or bar sets: Series children in place of value, up to 4, with a
  legend: <Series value="earned" label="Earned" color="green"/>. A
  missing value is a gap; past 64 elements it draws the first 64.
  name="pick" makes it pickable, as the usage chart is: a tap or drag
  writes the element's key (id, or key="slug") to that state, for a Data's
  when= or a show= to read.
Empty, Loading, Failed as="problem": inside a Data, shown for an empty
  answer, before the first one, and on an error ({{problem.message}}).
A Button or Item action calling a source runs it with its Params;
  then="set:…" (or toggle:, push:, present:, back) runs after it
  succeeds. One that asks first asks before running; others run on
  tap. A refusal shows under the button.
Connect: a button that opens the app's own sheet to connect target, and
  shows whether it is (connect_list tells you too). target="env:NAME"
  (capitals, digits and _) with scope, its .env file: plugin:<name>,
  plugins (the one every plugin reads), source:<handle> or
  project:.env.local; target="mcp:<server URL>" with name, what the
  server is listed and called by; or target="cli:<tool>" (gh, vercel…)
  for its sign-in. Optional title, note (where to find the value) and
  help (an https page). The user types a key or signs in there; it
  never passes through the chat or you. Never ask for a key or token in
  chat: draw a Connect.
