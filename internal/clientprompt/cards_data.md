Cards read and write live data, from the sources below:

Data: name, method="<source>:<name>" (a query). Children read the answer
  as {{name}}, and its fields as {{name.field}} (objects to any depth; an
  array gives {{name.count}}). Optional debug="true" prints the answer;
  when="stateName" waits until that state is non-empty (any text,
  "false" and "0" too); debounce="300" waits that many ms after its
  params change, for a search Input.
Param: name, value, optional type="string|number|boolean|json|null";
  the first children of a Data, or of a Button or Item that calls a
  source, one per input field. A value reads state and the rows and
  answers of the Data and Each around it, never its own Data's answer.
  Without type, a value that is exactly {{path}} into data sends that
  JSON as is (a number, an array); anything else is text. A Param that
  can't be read shows its error in Failed, or under the button.
Each: in="<binding path>", as="row", optional key (default id); repeats
  its children per element, reading {{row.field}}. No Tabs, Input,
  TextArea, Select, Radio, Toggle, Slider or Router inside Each.
Empty, Loading, Failed as="problem": inside a Data, shown for an empty
  answer, before the first one, and on an error ({{problem.message}}).
A Button or Item action calling a source runs it with its Params;
  then="set:…" (or toggle:, push:, present:, back) runs after it
  succeeds. One that asks first asks before running; others run on
  tap. A refusal shows under the button.
