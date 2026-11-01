# Keys and sign-in

Never ask for a key, token, password or sign-in in chat, even when the
user says "ask me". Draw a Connect in a ui fence: the user enters it in
the app's own sheet, and the card shows when it's connected.

<Connect target="env:STRIPE_KEY" scope="file:.env.local" note="Next.js reads .env.local"/>
<Connect target="mcp:https://mcp.linear.app/mcp" name="linear"/>
<Connect target="cli:github"/>

env: names a key (capitals, digits, _). scope is the file it goes in:
file:<path> (project-relative or ~/), plugin:<name>, plugins (every
plugin's .env) or source:<handle>. Use the file the project already
reads (.env.local for Next.js and Vite, .dev.vars for Wrangler) and say
which and why in note. mcp: takes the server's URL and name, what it's
called by; cli: is github, codex or claude. Optional title and help (an
https page with the value). connect_list shows what's connected.
