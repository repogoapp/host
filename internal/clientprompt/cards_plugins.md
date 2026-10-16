This project has plugins (.repogo/plugins/<name>/app.ts): small apps on the
user's environment with their own data. The `plugins` tool lists each
plugin's methods and input schemas; `plugin_call` runs one. Cards read
and write them live:

Data: name, method="plugin:<plugin>.<method>" (a query); subscribes and redraws
  whenever the answer changes, from any card, page or agent. Children
  read the answer as {{name}}, and its fields as {{name.field}} (objects
  to any depth; an array gives {{name.count}}). Optional debug="true"
  prints the answer; when="stateName" waits until that state is
  non-empty (any text, "false" and "0" too); debounce="300" waits that
  many ms after its params change, for a search Input.
Param: name, value, optional type="string|number|boolean|json|null";
  the first children of a Data, or of a plugin: Button or Item, one per
  input field. A value reads state and the rows and answers of the Data
  and Each around it, never its own Data's answer. Without type, a value
  that is exactly {{path}} into data sends that JSON as is (a number, an
  array); anything else is text. A Param that can't be read shows its
  error in Failed, or under the button.
Each: in="<binding path>", as="row", optional key (default id); repeats
  its children per element, reading {{row.field}}. No Tabs, Input,
  TextArea, Select, Radio, Toggle, Slider or Router inside Each.
Empty, Loading, Failed as="problem": inside a Data, shown for an empty
  answer, before the first one, and on an error ({{problem.message}}).
plugin:<plugin>.<method> — a Button or Item action that runs a method with
  its Params; then="set:…" (or toggle:, push:, present:, back) runs after
  it succeeds. A method the plugin gave a confirm question asks it before
  running; others run on tap. The plugin's refusal shows under the button.

Example:
```ui
<Col surface="card" padding="lg" radius="lg" width="fill" gap="md">
  <Input name="title" label="New plan" placeholder="Title"/>
  <Button label="Add" variant="primary" action="plugin:plans.plans.create" then="set:title=">
    <Param name="title" value="{{title}}"/>
  </Button>
  <List>
    <Section label="Plans">
      <Data name="plans" method="plugin:plans.plans.list">
        <Each in="plans" as="plan">
          <Item icon="doc.text" label="{{plan.title}}" action="plugin:plans.move">
            <Param name="id" value="{{plan.id}}"/>
            <Param name="status" value="complete"/>
            <Badge show="plan.status=complete" color="success">Done</Badge>
          </Item>
        </Each>
        <Empty><Item icon="tray" iconColor="gray" label="No plans yet"/></Empty>
      </Data>
    </Section>
  </List>
</Col>
```

Methods are named <plugin>.<method>; a collection adds
<plugin>.<collection>.list/get/create/update/delete. Check a method with
the plugins tool before a card names it. Plugins send data as it is
(dates as ISO or milliseconds); cards format it with filters.
