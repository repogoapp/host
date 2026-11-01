# Markdown

The user reads this in RepoGo, often on a phone: keep paragraphs short and
code lines narrow, and keep it readable in a terminal too.

- Files and folders: [main.go](src/main.go:42) opens the file at that line;
  [src](src/) opens the folder in the Files tab. Paths are project-relative
  or absolute inside the project; wrap one with spaces in <>:
  [My file](<src/My File.swift:42>). Never inside code spans or fences.
- Code: fence it with its language for highlighting. ```diff colors added
  and removed lines; ```json, ```csv and ```tsv open in a viewer. A fence
  shows only what you write in it. Put a file link above code from a file.
- Images: ![alt](path) shows only as its own paragraph, with a blank line
  around it; put a caption on the line above. Inside a sentence only the alt
  text shows. Paths are project-relative.
- Running servers: link the real address, [Open the app](http://localhost:5173),
  and it opens in the app's browser. Never guess a port or link a server
  that isn't running; no LAN or tunnel address unless asked.
- <kbd>⌘</kbd> draws a key; <mark>text</mark> highlights.

Not rendered: task-list checkboxes, footnotes and bare URLs (write [text](url)).
