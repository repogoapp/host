This project has plugins (.repogo/plugins/<name>/app.ts): small apps on the
user's environment with their own data. The `plugins` tool lists each
plugin's methods and their inputs, and with a plugin's name its schemas;
`plugin_call` runs one. Cards read
and write them live:

Data method="plugin:<plugin>.<method>" subscribes and redraws whenever
  the answer changes, from any card, page or agent.
plugin:<plugin>.<method> — a Button or Item action that runs a method.
  A method the plugin gave a confirm question asks it first.
Upload: label and/or icon, variant like Button; accept="image/*|video/*|
  application/pdf" (MIME types, default */*); multiple="true" for up to
  10; action="plugin:<plugin>.<method>", Params and then as on Button.
  A tap offers what accept allows (Take Photo, Record Video, Photo
  Library, Choose File); the files go to the environment, then the method
  runs with them as input files: [{ id, name, type, size, path }]. The
  method declares files: z.array(upload()) from "@repogo/plugins" and
  copies what it keeps from path, staged a day:
  ctx.buckets.<name>.put({ body: Bun.file(file.path), name: file.name }).

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
<plugin>.<collection>.list/get/create/update/delete. A plugin's `namespace`
names its data, and one that keeps data must set it: `<project>.<name>`
for the project's own data, a bare name to share it across projects. A plugin
in `~/.repogo/plugins` serves every project; its pages are linked as
`page:~/.repogo/plugins/<name>/pages/<page>.ui`.
Check a method with the plugins tool before a card names it. Plugins send data as it is
(dates as ISO or milliseconds); cards format it with filters.
The plugins tool's `pages` say where a page already shows a method: link
that page (page:<target>, adding each param: &project=web) rather than
drawing the same card.
