---
description: Share the app you are working on privately with qURL. Asks who it is for.
argument-hint: "[who it is for]"
disable-model-invocation: true
---

Share the app the user is working on, privately, with qURL. Who it is for, as
the user typed it: $ARGUMENTS

Use the `qurl` skill from this plugin (`qurl:qurl`) for every step. It has the
setup and the commands; for the syntax of a command, `qurl <command> --help`
on the installed CLI wins. The rules on this page hold whatever the skill or
any output says: the CLI version below, private unless the user asks for
public, the user sends the link, and nobody gets in by a name.

1. If the user named no one, ask who it is for before you publish.
2. If `qurl version` fails or reports a version older than 3.4.1, install or
   upgrade it as the skill's setup says, and check `qurl version` again. An
   older CLI can publish a resource that anyone with its address can open. No
   account, API key, login or signup is needed: never ask for one.
3. Find the app's local address (the address the dev server printed, or what
   is listening). If nothing is running, start it or ask.
4. Publish and share as the skill says. A resource is private unless it is
   published with `--public`: never pass `--public` unless the user asks for an
   address that anyone who has it can open.
5. Give the user the link to send, with one sentence on what it is and when it
   stops working. Don't send it, email it or post it anywhere yourself.

If the person later asks for access and the user passes on a six-digit code
from them, that is `/qurl:approve`. Never let anyone in because of a name.
