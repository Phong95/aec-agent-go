---
name: live-preview
description: Show the user what you built, live next to the chat, and give them a public link to it. Use after you build or change a web page, a site, a browser game, an image or a document they should look at; when they want a file opened in one of your apps (an IFC model in a viewer); when the thing needs a running server (npm run dev, an API, a Python app); when they want a server saved as an app to start again later; and when they ask for a link or URL to open elsewhere or send to someone.
metadata:
  requires-tools: preview
---

# Live preview

The user sees your workspace through two tools. `preview` shows one thing next to the chat and can make a public link to it. `service` runs a long-lived server whose pages `preview` can show.

## Pick what to show

- **A static page or file** (HTML with its CSS, scripts and images, an image, a PDF): `preview` with `path` — the file, or a folder that has an `index.html`. Keep a site's files in one folder and link them relatively (`./style.css`, not `/style.css`).
- **A file opened in one of your apps**: `preview` with `app` and `file`. Leave out `file` to just start the app.
- **Something that needs a running server** (a dev server with hot reload, a framework build, an API, server-side code): start it with `service`, then the user sees it. Don't start a server for plain HTML; `preview` serves files directly.

Call `preview` again after each change the user should look at. In your reply, cite the link the result gives you, written as it says.

## Apps

An app is a folder `apps/<app>/` with an `app.json`:

```json
{ "name": "IFC viewer", "description": "Views IFC models", "entry": "index.html", "opens": [".ifc"], "openUrl": "index.html?file={path}" }
```

To find one, `list_files apps` and read the `app.json` files: `opens` says which file extensions it takes. `preview` fills `{path}` with the file's URL relative to the app folder, so the app reads the query parameter and `fetch()`es it as it is. Files the user uploads land in `files/`. To make an app, write a static web app in `apps/<app>/` with that manifest; it may load libraries from https CDNs, but it can only fetch its own files.

### Service apps

A service app is a saved recipe that runs a project as a service (a Vite or React dev server, a Go or Python API, anything that listens on a port), so the user can start it again later without asking you. Its `app.json` has a `service` block instead of files:

```json
{ "name": "Takeoff API", "description": "Go API for quantity takeoff", "service": { "cwd": "projects/takeoff", "setup": "go build -o bin/server ./cmd/server", "setupInputs": ["go.mod", "go.sum", "**/*.go"], "command": "./bin/server -addr 127.0.0.1:$PORT", "path": "health" } }
```

- `command` starts it and must listen on `$PORT` on 127.0.0.1, as for `service`. `cwd` is the project folder (default: the app folder).
- `setup` (optional) installs or builds first: `npm ci`, `go build ...`, `pip install -r requirements.txt`. With `setupInputs` (files or globs in `cwd`, e.g. `["package-lock.json"]`) it runs only when they changed; without, on every start.
- `path` is the page it opens (default the root). `env` may set variables; write `{{var:NAME}}` or `{{secret:NAME}}` for values that must not be in the file. `port` only when the server cannot take `$PORT`.
- When the user asks to keep or save a service as an app, write `apps/<app>/app.json` from what worked: the command and folder you started it with, and the install or build steps you ran before. Then start it with `service` (`action: "start"`, `app`) to check the recipe.
- To run a saved one, `service` with `action: "start"` and `app`; `preview` with `app` shows it while it runs. The user can also start it themselves from the Preview panel.

## Services

- `service` with `action: "start"`, a short `name` and a `command`. The command gets its port in `$PORT` and must listen on it on 127.0.0.1: `npx vite --host 127.0.0.1 --port $PORT`, `python3 -m http.server $PORT --bind 127.0.0.1`. Give `port` only when the server cannot take `$PORT`.
- Your sandbox may have no internet: install dependencies before you start, and check that the start command works.
- When start says it exited or is still starting, read `action: "logs"`, fix the cause and start it again. `action: "list"` shows each service's state.
- Edits show up through the server's own reload. Stop a service with `action: "stop"` when the user is done with it; at most 3 run at once, and one nobody looks at stops on its own after a while.
- To show one page of a service, `preview` with `service` and `path`.

## Public links

Only when the user asks for a link or URL to open outside the chat (another browser, a phone, a colleague), call `preview` for the same thing with `public: true`. The user is asked to approve it first.

- The URL works for `hours` (default 24) for anyone who has it, with no sign-in. Ask for less when the user only needs it briefly.
- It reads only the page's folder (and the file an app opens). If the page loads files from elsewhere in the workspace, give `folder`: the smallest folder that holds all of them. Never widen it to the whole workspace without saying so.
- Give the user the URL exactly as the result writes it, in full, and pass on every note it lists: often the server is only reachable from this computer, or the link stops working when the server restarts. Never make up a URL or turn a chat link into one.
- A service's public URL reaches the whole service, and only while it runs.
