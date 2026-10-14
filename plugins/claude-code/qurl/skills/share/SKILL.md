---
description: Share the app you are working on privately with qURL. Asks who it is for.
argument-hint: "[who it is for]"
disable-model-invocation: true
---

Share the app the user is working on, privately, with qURL. Who it is for, as
the user typed it: $ARGUMENTS

Use the `qurl` skill from this plugin (`qurl:qurl`) for every step. It has the
setup, the commands and the rules; where this page and the skill differ, the
skill and `qurl <command> --help` on the installed CLI win.

1. If the user named no one, ask who it is for before you publish.
2. If `qurl version` fails or reports a version older than 3.0.0, install it
   as the skill's setup says. No account, API key, login or signup is needed:
   never ask for one.
3. Find the app's local address (the address the dev server printed, or what
   is listening). If nothing is running, start it or ask.
4. Publish and share as the skill says. A resource is private unless it is
   published with `--public`: never pass `--public` unless the user asks for an
   address that anyone who has it can open.
5. Give the user the link to send, with one sentence on what it is and when it
   stops working. Don't send it, email it or post it anywhere yourself.

If the person later asks for access and the user passes on a six-digit code
from them, that is `/qurl:approve`. Never let anyone in because of a name.
