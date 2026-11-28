# matrix-edupage

A Matrix ↔ [EduPage](https://www.edupage.org) puppeting bridge, built on [mautrix-go](https://github.com/mautrix/go)'s
bridgev2 framework. It bridges the messages of a school's EduPage (pupils, parents and teachers write to
each other there) and its notices: grades, homework, sign-ups, events and read confirmations.

It is not part of the mautrix project, nor of EduPage or aSc Applied Software Consultants. Issues go to this
repository.

**Status:** early. Everything in [ROADMAP.md](ROADMAP.md) is covered by an end-to-end test against a fake
EduPage and a throwaway homeserver; this Go bridge has not yet run against a real school. Its protocol is
ported from the Python library [edupage-api](https://github.com/EdupageAPI/edupage-api), which has no Go
counterpart.

## How it works

* **Sign-in** is the same as on edupage.org: user name, password and optionally the school (the first part
  of `<school>.edupage.org`; without it EduPage's `login1` finds the school). When EduPage asks for a second
  factor there are two login flows: *password* waits until the sign-in is confirmed in the EduPage app,
  *password_code* has EduPage mail a code and asks for it. A captcha (after too many wrong passwords) has to
  be solved once on edupage.org.
* **Session:** the bridge keeps EduPage's session cookie and, to sign in again when it expires, the password,
  in its database. When the password stops working the login goes to `BAD_CREDENTIALS` and you sign in again.
* **Polling:** EduPage has no push, so the bridge reads the timeline every `poll_interval` (5 min by
  default), re-reading the last `recent_days` days so that changed notices arrive as edits. The first poll
  after signing in reads the whole school year once.
* **Rooms:** every person you write with gets a direct chat. A thread on EduPage is the first message and its
  replies, which arrive as Matrix replies to it; a message you sent to a class or a group has its own room.
  Notices go to one room, *EduPage*, each from the teacher who wrote it, with a link to the page where it is
  answered (signing up or confirming stays on EduPage).
* **Files:** attachments of messages are links to EduPage (they need its session to open); the material of a
  recent homework (its text and files, read once) comes with the homework notice, the files as Matrix media.
* **Sending:** in a person's room a message starts a new EduPage message to them (a pupil alone, without
  their parents); a Matrix reply answers in that thread, also in a class room. Files go up as EduPage's own
  form uploads them (25 MB each). EduPage has no edits, deletions, reactions, typing or read receipts for
  messages, so the rooms say so.
* **New chats:** the contact list (`contacts`, `search_users`) is whom EduPage lets you write to, by your
  role: a pupil gets their class, their subject groups, their classmates and the teachers; a teacher every
  class, group, pupil and parent, the other teachers, all teachers and all pupils; a parent the teachers. A
  person's chat starts with `create_dm`. A new message to several of them, or to a class or group, is
  `create_group` of type `message` with the contact ids as participants (`studtrieda7` a class,
  `studplan12` a group, `studentonly5` a pupil, `ucitel3` a teacher, `rodic9` a parent, `ucitel*` and
  `studentonly*` everyone); each contact's second identifier (`edupage:Trieda7`, `edupage:Student5`)
  also reaches the parents. `resolve_identifier` takes the same ids, several joined by `;`. That room's
  messages each start a new EduPage message to all of them, checked against the list again when sent. A
  teacher's message can carry edupage.org's choices as
  `"io.github.gooder24.edupage.options": {"important": true, "replies": "all" | "author" | "none"}`
  (a read confirmation; who may answer). A room the bridge made for a message you sent to a class
  only answers in its threads.

## Setup

You need Go 1.26 or newer and a Matrix homeserver that supports application services.

```sh
go build -o matrix-edupage ./cmd/matrix-edupage
./matrix-edupage -e                 # writes config.yaml from the example
$EDITOR config.yaml                 # homeserver, database, permissions; the network section is optional
./matrix-edupage -g                 # writes registration.yaml; add it to your homeserver
./matrix-edupage
```

Then send `login` to the bridge bot (`@edupagebot:your.server`) and follow its instructions, or use the
bridgev2 provisioning API (`/_matrix/provision/v3/login/start/password`). Turn on double puppeting if your
own EduPage messages should come from your Matrix account rather than from a ghost.

### With Beeper

Like any bridgev2 bridge, it can run on your own machine and connect to your Beeper account with
[bbctl](https://github.com/beeper/bridge-manager):

```sh
bbctl login
bbctl config --type bridgev2 -o config.yaml sh-edupage
./matrix-edupage -c config.yaml
```

bbctl writes everything except the `network` section; the bridge adds it with its defaults when it first
starts. This path has not been tried yet; reports are welcome.

## Configuration

The `network` section of `config.yaml`:

| Key | Default | Meaning |
|---|---|---|
| `edupage_url` | `https://{school}.edupage.org` | Where a school's EduPage is. Change only for testing. |
| `poll_interval` | `5m` | How often to read EduPage. |
| `recent_days` | `7` | Days each poll re-reads, for changed notices. |
| `initial_days` | `0` | Days the first poll reads; 0 is the whole school year. |
| `settle_days` | `7` | On that first poll, older messages are marked read (with double puppeting). |
| `arrivals`, `meals` | `false` | Bridge arrival and meal notices too (EduPage sends many). |
| `max_file_size` | 25 MiB | Larger files are not bridged. |

## Differences from Weft's EduPage bridge

This bridge replaces the Python bridge of [Weft](https://github.com/Gooder24/weft), with two differences:
each person has their own room instead of one EduPage room, and the parts that only Weft uses stay out:
the grade book and notice rules in account data, the "Testy" calendar of upcoming tests, the nudge that has
the timetable read again after a substitution, each teacher's colour, and the heartbeat Weft shows. Ghost IDs
(`edupage_ucitel123`) are the same. Content keys under `sk.karchi.edupage.*` (`kind`, `to`, `actions`,
`signin`) carry what Weft draws; other clients ignore them.

Sending works the Matrix way: a message in a person's room goes to that person, a reply answers its thread,
a file is its own media event, and Weft's "Komu" list is the contact list with `create_group`. Weft's
composer for the Python bridge instead sends an `m.text` with an explicit recipient and attachment list
(`sk.karchi.edupage.recipients`, `sk.karchi.edupage.options`) into the one shared room; it has to change
before Weft can switch.

## Tests

```sh
go test ./...                                         # unit tests; the end-to-end test is skipped
TUWUNEL=/path/to/tuwunel go test ./tests/e2e -v       # bridge + throwaway Tuwunel + fake EduPage
```

The end-to-end test starts the bridge as its own process between a temporary
[Tuwunel](https://github.com/matrix-construct/tuwunel) homeserver and `internal/fakeedupage`, signs in
(password, school found through login1, confirmation in the app, mailed code, an expired session, a changed
password) and checks messages, notices, sending, room features and the teacher search. Nothing reaches
edupage.org or any other server. Not covered there: a successful sign-in through the old `edubarLogin.php`
form (the fake only refuses on it), double puppeting, and arrival and meal notices turned on.

`E2E_MEASURE=5m` measures the bridge signed in and polling every 30 s against the fake: on a laptop it used
about 31 MB of memory and 0.4–0.9 s of CPU per hour. Weft's Python bridge used 33 MB and about 1.8 s per
hour polling the real EduPage every 5 minutes; the workloads differ, so take it only as "about the same
memory, less CPU".

## License

AGPL-3.0-or-later, like the mautrix bridges.
