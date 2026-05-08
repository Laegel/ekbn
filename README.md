# ekbn - Embedded Kanban

[screenshot.png](screenshot.png)

A lightweight local tool for project management.
You can edit the cards with your favorite text editor or open the web application

## Architecture

Each folder represents a column.
1 task = 1 Markdown file in a column.
1 server app reading and writing files, written in Go.
1 front-end app, written in JS with DaisyUI.

## Server & UI Capabilities

- Create card
- Update card
- Move card
- Delete card
- Create column
- Move column

## Customization

You can create a `config.yml` file next to the binary and set the following properties:

```yml
theme: dark
folder-name: columns
port: 8080
```

The theme can be configured via a `custom.css` file. ekbn is using DaisyUI under the hood so defining themes is as simple as providing custom variables.
See the `custom.css` file from the repo.
