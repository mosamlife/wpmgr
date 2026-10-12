# ADR-065: AI writes are decided by tier; a person sets the tiers

**Status:** Accepted (2026-10-09); takes effect in two merges, see [What takes effect when](#what-takes-effect-when) · **Date:** 2026-10-09
**Supersedes/relates:** [ADR-061](./ADR-061-assistant-surface-phase-1.md) (superseded in part: the last paragraph of Decision 2, "No automation escape hatch, ever", the Consequences bullet "No automation may ever approve", and amendment A5's restatement of that bullet; everything else in ADR-061 stands), [ADR-062](./ADR-062-assistant-surface-phase-2-content-operations.md) (superseded in part: the per-call approval sentence in A-062-5), [ADR-060](./ADR-060-phase-order-safety-before-capability.md) (this work sits in its "constrained automation" phase, the same phase as ADR-061).

ADR-061 made every AI write wait for a person's click. This ADR keeps the claim
that rule existed to protect, that a person authorized every change and the
record can show who, and changes how a person gives that authorization: one
change at a time, or in advance, for a named kind of change on a named site.

AI changes are sorted into three tiers.

- **Auto.** The change runs at once, because a named person chose in advance
  that this kind of change may run without asking. The record names that
  person and that choice.
- **Ask.** The change waits for a person to approve it in the dashboard, as
  every change does today.
- **Never.** The change is not offered to the AI at all.

The AI cannot move a change between tiers, raise a limit or widen a setting,
and no text it reads or writes changes the answer.

This ADR records decisions and the contracts they create. It is not a build
plan.

**How it lands.** The work ships as two merges on one branch. **Merge A**
delivers the goal that prompted this ADR and only that: an AI working in a
coding client's auto mode creates and edits its own drafts with no dashboard
click, and everything else waits for a person as it does today. **Merge B** adds
Full auto, sessions, cache purges on the same decider and the budgets for
changes visitors can see. Each decision below says which merge puts it in
force. A decision marked for Merge B is decided now and is not in force until
Merge B ships; for that part, ADR-061 stands as written.

**Accepted 2026-10-09 by owner ruling.** Acceptance records the rulings the
owner made on the approved design that day. It does not mean any of it is
built: [What takes effect when](#what-takes-effect-when) says which merge puts
each decision in force, and until a decision is in force, ADR-061 governs that
part.

---

## Context

In a coding client's auto mode an AI works through a long run of tool calls
with no person watching each one. ADR-061 put one stop in that run: every
change waits for a click in the WPMgr dashboard. That stop has two costs, and
both have become the main cost.

**It is the bottleneck.** An AI that drafts a page cannot continue until
someone is at the dashboard, so the work stops whenever the person looks away.

**It defeats itself.** ADR-061's own account of what was expensive to learn
names approval fatigue as the thing that defeats the gate. A click that is
nearly always "approve" teaches a person to click without reading, and the most
frequent change an AI makes, a new draft that no visitor can see, is the one
where the click protects least.

On 2026-10-09 the owner ruled that AI writes are no longer all decided by a
click. They are sorted into tiers by the kind of change. A person decides in
advance how much of the Auto tier each site gets. The default covers the AI's
own drafts and nothing else. The Ask tier stays for what should not run
unattended. The same ruling puts running code on a site in scope for the
product, under a boundary this ADR records and does not design (Decision 11).

---

## Decision 1: The AI never raises its own permissions; a person decides in advance

**The AI can never raise its own permissions. A person decides in advance how
much it may do on each site, and every change is snapshotted, undoable where it
says so, audited and visible.**

Four properties follow.

- **An automatic approval names the person whose choice made it.** The record
  of a change that ran without a click holds which setting or session allowed
  it, which person set it, and when. It is never recorded as that person's
  decision on the request, because the person did not decide the request.
- **A choice stands only while its owner could still make it.** When a change
  is approved automatically, the person whose setting or session allows it must
  still be able to approve that very change by hand, must still hold the
  authority to have made that choice, and must have an active account. The
  person who allowed the connection to run changes automatically must hold the
  authority to do that, with an active account, too. If any of them has been
  removed, demoted, restricted to some sites, had a site share withdrawn or
  been disabled, or if the check cannot be completed, the change waits for a
  person. The setting stays on record and does nothing until a person with the
  authority chooses it again.
- **The decision reads stored facts only.** The class of a change comes from
  the catalogue entry or reviewed route, which only a superadmin writes, from
  the checked precheck, and from the control plane's own record of what it
  created. It never comes from the AI's arguments or from any text on a site
  (ADR-061 A13). The decision is handed the request's id and nothing the AI
  sent.
- **Every automatic change is visible and undoable.** It carries the snapshot
  and Undo that ADR-062 already requires of every write, is listed in AI
  activity as "Ran automatically", and shows who or which setting allowed it.
  Where Undo would not be exact, the change is on the Ask list (Decision 3).

---

## Decision 2: Changes are sorted into classes, and a site's setting maps each class to a tier

The engine never asks "is this a write". It asks which **change class** a
request is in, then looks the class up in the site's setting. A site has one
setting, chosen by a person:

- **Ask every time.** Every class is Ask.
- **Auto for AI drafts.** The default for a site that has AI editing on. The
  AI's own drafts, and changes to them, are Auto. Everything else is Ask.
- **Full auto on this site.** Also runs changes to unpublished content that is
  not an AI draft, edits to published pages, publishing and scheduling, and
  plugin and theme updates. Merge B.

| Class | Meaning | Ask every time | Auto for AI drafts | Full auto | A session can cover it (Merge B) |
|---|---|---|---|---|---|
| `ai_draft` | Creates a new draft, or changes an AI draft whose checked status is exactly `draft` | Ask | **Auto** | **Auto** | yes |
| `operational` | Changes no content and loses nothing; today that is a cache purge | Ask | **Auto** (Merge B) | **Auto** | yes |
| `unpublished` | Changes content that is not live and is not an AI draft: a person's own draft, a post pending review | Ask | Ask | **Auto** | yes |
| `live` | Changes something visitors can see now | Ask | Ask | **Auto** | yes |
| `publish` | Changes visibility or makes it happen later: publishing, scheduling | Ask | Ask | **Auto** | yes |
| `update` | A plugin or theme update with a backup and a health check | Ask | Ask | **Auto** | yes |
| `always_ask` | The Ask list (Decision 3) | Ask | Ask | Ask | no |
| Never | Not runnable at all | not offered | not offered | not offered | no |

The default covers the AI's own drafts only. A draft a person made, and a post
pending review, wait for a person unless the site is on Full auto. An **AI
draft** is a post that the control plane's own record shows an AI request
created, that has not been undone, and whose checked status is exactly `draft`.

**How the class of one request is found.** A class is stored on the catalogue
entry or reviewed route. It is data a superadmin writes through an audited
function. A request is classed by these rules, in this order:

1. A stored `always_ask` is final. Nothing narrows it or lifts it.
2. If the checked precheck reports that Undo would not be exact, the request is
   `always_ask`, whatever its stored class.
3. A route or ability that edits an existing post may store "decided by the
   target's status" instead of a class. That value is never itself a class, and
   no tier is defined for it. The target's checked status decides, compared
   exactly as WordPress reports it: `publish` and `private` are `live`;
   `future` is `publish`; `pending` is `unpublished`; `draft` is `ai_draft`
   when the post is an AI draft and `unpublished` when it is not. Any other
   status, or none, waits for a person.
4. Any other stored class is used as it is. The target's status never changes
   it.
5. A class that is missing or unknown waits for a person. A new catalogue entry
   starts on the Ask list until a superadmin classes it.

No rule compares two classes by strictness. A checked fact never lowers a
stored class: it can only move a change onto the Ask list (rules 1 and 2), and
it decides a change whose stored value was never a class (rule 3).

**Classes at acceptance.** `wpmgr/page-create` is `ai_draft`. `wpmgr/rest-write`
on a page or post is decided by the target's status. A cache purge is
`operational`, and waits for a person until it moves onto the decider in
Merge B. Every ability admitted later states its class when it is admitted, and
an ability that cannot be classed waits for a person. No ability is classed
above Ask unless its catalogue entry records a snapshot for it, and the
database refuses the row otherwise.

**Never.** Some things are not offered to the AI at all: a catalogue entry
marked denied, the agent's own list of forbidden namespaces and capabilities,
capabilities the WPMgr content service user never holds, and running code
(Decision 11). Never is enforced in code, and by superadmin-written rows that
refuse a request before the engine sees it. No site setting, connection
setting or session can move a change out of it, and the agent applies its own
list whatever the control plane approved.

The matrix is open to new classes: the code-execution design can add one
(Decision 11) without contradicting this ADR. It exists in code in exactly two
places, the decision code and the database function the approving statements and
the backstop call, and a test checks every pair of setting and class against
both.

---

## Decision 3: The Ask list is a named list

These always wait for a person, in every setting and every session:

1. permanent deletes;
2. users, roles and passwords;
3. the site address;
4. installing, switching on or removing plugins and themes;
5. WordPress core updates;
6. any change whose Undo WPMgr found would not be exact, whatever its stored
   class;
7. any catalogue entry a superadmin has not classed yet.

The list is stated by name here, in the dashboard and in the text the AI is
given. It is deliberately not stated as "anything WPMgr cannot undo". Full auto
does run some changes whose effects Undo cannot fully take back (Decision 8),
so reversibility is not the rule; the list is. No setting, session or
connection permission covers it.

---

## Decision 4: Only a signed-in person can loosen an AI control

**Loosening an AI control needs a signed-in person.** That covers raising a
site's setting, allowing a connection to run changes automatically, starting a
session, turning on Full auto, and every other control that widens what an AI
may do. The check is made in the service layer, not only in route middleware,
because middleware alone would let an API key through. An API key, an MCP
token and the AI itself cannot loosen anything.

**Tightening needs only authorisation.** Lowering a site to Ask every time,
setting a connection to never, ending a session and revoking a connection are
open to any authorised principal, so a key or an incident script can always
make the system safer.

| Control | Who may raise or turn it on | Who may lower it |
|---|---|---|
| Site setting: Ask every time, Auto for AI drafts | A signed-in operator, admin or owner with access to that site (the same permission that turns AI editing on) | Any authorised principal, to Ask every time |
| Connection switch: "where each site allows it" or "never (always ask)" | A signed-in owner or admin who is a full member of the organisation | Any authorised principal, to never |
| Full auto on this site (Merge B) | A signed-in owner or admin who is a full member, with a typed confirmation and a step-up | Any authorised principal |
| Session (Merge B) | A signed-in person who may edit content on that site | Any authorised principal |

A member who is limited to some sites can never choose Full auto or the
connection switch, whatever share they hold. Full auto needs the site's address
typed as a confirmation, and a step-up that proves the person is present: their
password, otherwise a code from their second factor, otherwise a sign-in within
the last ten minutes. A typed address alone is never enough. Turning Full auto
on or off emails every owner and admin, and no setting switches that email off.

**The AI cannot reach any of it.** No tool in the AI's registry writes a
setting, a switch or a session. The code that serves the AI's tools does not
import the code that changes them, and a test fails the build if it does. An
MCP bearer token on any loosening route is refused. A page, a title, a skill or
a prompt that asks for more changes nothing, because that text is not consulted
when the question is decided (ADR-061 A13). The database backs the service
layer: a site's setting and a connection's switch cannot be raised by a
statement that carries no user, or a user other than the one recorded as the
setter.

**A residual is stated plainly.** An AI that drives the person's own signed-in
browser cannot be told apart from the person. The typed confirmation, the
step-up and the email reduce what such an AI can do silently. They do not
remove the risk, and this ADR does not claim they do.

---

## Decision 5: How an automatic approval is recorded, and who can write one

**The approval code in the control plane is the only approver, and it records
an automatic approval as what it is.**

- Every request is created waiting, and the database refuses a request
  inserted already decided. After the request exists, the approval code is
  called with its id and reads everything else itself.
- The code that serves the AI's tools holds no statement that approves
  anything, and a test fails the build if one appears (ADR-061 Decision 2,
  restated as a check).
- The request records its approval source: `person`, `policy` (a site's
  setting) or `session`. For `policy` it records the setting, the version of
  the setting, the person who set it and when. For `session` it records the
  session and the person who approved it. It records the class the catalogue
  stored and the class the request was decided in. These are written once, at
  the decision, and are immutable afterwards.
- The audit row is written in the same transaction and is fail-closed, as
  ADR-061 Decision 2 and A10 require. Its actor type is `policy`, with the site
  as the actor, or `session`, with the session as the actor. It is never
  `user` and never `assistant`, and the person who set the setting is never
  named as the decider. Its metadata holds the setting and its version, both
  classes, each setter check with the rule applied and its result, and the
  counts the budgets saw.
- The database enforces the line between a person and a policy. A person's
  approval must carry that person's own user id on the transaction. An
  automatic approval must carry none, and is refused in any transaction that
  has one, so a lookup of a setter's identity that strayed into the approving
  transaction fails loudly instead of writing a human's approval.
- The database re-checks the policy when the approval is written, and again
  just before the request is sent to the site. An automatic approval is sent
  only if the setting it relied on is unchanged and still allows the class, the
  connection's switch is still on, and the catalogue's class for the ability is
  still the one the request was decided in. Otherwise the request closes
  unsent, with the reason. A person's approval is not closed by a later change
  of class, because the person saw the card.
- A person who lowers a setting wins every race. A decision running at that
  moment, and a request already approved but not yet sent, both see the lowered
  setting, and neither runs.
- A request that has not been decided within two minutes of its creation is
  never approved automatically later. It waits for a person. No request is
  approved under a setting that did not exist when it was made.

The person's own path is unchanged, except that it records its approval source.
A person's approval is still digest-bound, still re-checks the permission the
request needs, and is still written with its audit row or not at all.
ADR-061 Decision 3 applies to the cards of automatic changes as it does to
waiting ones: every fact on a card is WPMgr's, the "allowed by" line is built
from values stored on the request and never re-read from the site, and text the
AI chose appears only in its one quarantined slot.

---

## Decision 6: A session lets a person allow a kind of change for a short time (Merge B)

*Takes effect with Merge B.*

A waiting card can be approved with **Approve and allow {kind} for 30
minutes**, and the Requests page can start the same grant without a waiting
card. The same kind of change on that site, from that connection, then runs
without asking until the time or the change count runs out. A session is the
general form of a bounded run of automatic changes. A later design that needs
one uses this grant and does not define its own.

A session is bounded five ways.

- **One site.**
- **One connection.**
- **Named kinds of change**, only those the person could approve by hand on
  that site, and never a kind on the Ask list.
- **15 or 30 minutes**, and no other length.
- **40 changes.** Both limits are counted exactly: two approvals cannot rely on
  one increment.

Only a signed-in person starts a session, with the permission that approving on
that site takes. A session cannot cover a connection whose switch is on never,
or whose setter has lost the authority to allow it (Decision 1). The session's
approver is held to the same check as a site's setter on every change.

A session ends when the person ends it; when its time runs out; when its
change count is used; when the site is set to Ask every time; when the
connection is revoked or set to never; when AI is paused for the organisation;
when something it covers changes; or when one of its changes is undone. It is
therefore not an approve-all, and ADR-061's rule that there is no bulk approve
and no select-all is unchanged.

---

## Decision 7: Budgets are fixed server numbers, counted durably, and fall back to Ask

Each connection has limits on automatic changes. Draft work gets generous
limits. Changes visitors can see get tight ones. The numbers are constants in
the server and are shown read-only on the connection screen. There are no
editable limits: nobody, including the person who owns a connection, can raise
one.

Counts are taken over the request rows themselves, per connection and per
organisation, over a rolling hour, so they survive a restart and cannot be
reset by the AI. They are exact because every automatic approval in an
organisation, from either request table, is decided under one lock.

Over a limit, a request is not refused. It is created and waits for a person,
with a closed reason. The AI is told that its automatic changes are used up for
now, when they resume if that is known, and not to split the work or try other
sites to get around the limit. It is never told how much quota remains.

| Bucket | Counts | Opening limit per rolling hour | Over the limit | Merge |
|---|---|---|---|---|
| Draft changes per connection | `ai_draft`, `unpublished` | 600 | waits; no email | A |
| Sites with draft changes per connection | `ai_draft`, `unpublished` | 30 | waits; no email | A |
| Cache clears per connection | `operational` | 200 | waits; counted in no sites cap | B |
| Visible changes per connection | `live`, `publish`, `update` | 60 | waits | B |
| Sites with visible changes per connection | `live`, `publish`, `update` | 3 | waits; email to owners and admins; the connection switches to never | B |
| Sites with visible changes per organisation, all connections | `live`, `publish`, `update` | 10 | waits; email to owners and admins | B |

"Automatic" in these counts means approved by a setting. A session's changes
count against the session's own limit.

The numbers are opening values, inferred and not measured, and are to be
revisited after a month of activity data. Changing a number does not need a new
ADR. Making a limit editable, or letting a connection or a person raise one,
does.

**The latch.** When a connection reaches the limit on sites with visible
changes, it is switched to "never (always ask)" until a person switches it
back, and owners and admins are emailed how to do that. This is a tightening,
so the system may make it with no person present (Merge B).

Caps that bound the queue are a different thing and keep refusing: how many
requests may wait, and how many a connection may create per day and per site per
hour. They are re-sized for automatic work.

---

## Decision 8: What Undo cannot take back

Undo is a promise about the site's content, not about the world. Visitors,
search engines and subscribers see a change before anyone undoes it. A publish
can send emails, pings and feeds. A plugin's own database change is not reversed
by restoring its files. So the default holds back every class that touches what
visitors see or what the site runs: `live`, `publish` and `update`. Only a site
set to Full auto runs them automatically, and the confirmation that turns Full
auto on names what each of them cannot take back.

A change is classed by what visitors can see, not by the status of the record
that holds it. WordPress serves an uploaded file at its public address as soon
as it lands, whatever the attachment's status, so importing a picture into the
media library is `live` from the first byte.

**No read whose output may be non-public is admitted until the private-read
fall-back ships.** WPMgr's own reads return published, unprotected content
only. Full auto lets an AI publish what it has read. If it could also read
content that is not public, one injected instruction could move private content
into public view. From Merge B, the database refuses to admit a read that is
not one of WPMgr's own, and while one is enabled, automatic approval of changes
visitors can see is held, so every such change waits for a person. The hold
lifts when the last such read is disabled or when the fall-back ships and
removes the guard and the hold together.

---

## Decision 9: In-client Ask is a link now, and never replaces the dashboard proof

When a change waits for a person, the tool result says so in a fixed shape: a
waiting state, `approval: "ask"`, a closed reason, an absolute link to the card,
and a message that is a constant in the code. Reads carry `approval: "none"`,
and a change that ran carries `approval: "auto"` (or `"session"` from Merge B).
The result of an automatic change is `done` and is returned in the same call.

No site text or person text is interpolated into any message, and no message
says who set a policy. The AI is told to give the person the link, not to wait
in a loop, and not to make the same change another way. The link opens that
card with Decline focused.

Later layers, a tool annotation that asks the client to prompt the person, and
URL-mode elicitation, are not built. If they are, they only deliver the person
to the dashboard. **The proof stays the dashboard click**: a signed-in person,
the permission the request needs re-checked, the presented digest in the
approving statement, and the fail-closed audit row. ADR-061 A6's rule that
elicitation and sampling are never on the approval path is unchanged.

---

## Decision 10: The default, and how existing sites move

- **Turning AI editing on** for a site that has never had a setting chosen sets
  Auto for AI drafts, in the name of the person who turned it on. The screen
  says so above the button. Turning it on again never changes a setting a
  person has chosen.
- **Existing sites.** On release, every site that already has AI editing on
  moves to Auto for AI drafts, with the person who turned AI editing on as the
  setter. Where that account has since been deleted, the site stays on Ask every
  time with no setter. Where that person no longer has the authority to have
  chosen it, the setting is on record and every change waits for a person until
  someone chooses again (Decision 1).
- **Telling people.** Each organisation's owners and admins get one email
  listing the sites that moved. It carries no model text and no approve link.
  On each moved site, the AI editing card says so and offers "Keep asking every
  time" and "Keep Auto for AI drafts" until a person chooses either. Either
  choice records the setting as that person's own.
- **Connections.** A connection created by a person, in the dashboard or by
  consent, starts with its switch on "where each site allows it" and that
  person as its setter. A connection created with an API key starts on "never
  (always ask)", because no person is behind it. A connection can only narrow a
  site's setting, never widen it.
- **Removing a member** revokes the AI connections that member created. That is
  its own small change after Merge A. Until it ships, a connection whose setter
  is no longer a member runs nothing automatically (Decision 1).

---

## Decision 11: Running code is outside the matrix (decided, designed separately)

Arbitrary PHP and shell execution on a site are not in this ADR's matrix, and
nothing in either merge can run them. They stay in Never until their own design
is accepted. The owner has ruled that they are in scope for the product. This
ADR records the boundary that design must fit inside, so this matrix does not
have to be rewritten to admit it:

- **Builds.** They exist only in the managed and GitHub builds of the agent. The
  wordpress.org build never contains them, and a check on that package proves
  it.
- **Sites.** They run only on a site set to Full auto that also has its own code
  switch turned on. Full auto alone does not turn them on.
- **Order.** They run only after a restore point has been taken.
- **Record.** The code and its output are recorded in full.

That design adds its own change class and its own ADR. Until that ADR is
accepted, ADR-061 Decision 6's exclusion of shell execution and command-runner
tools stands.

---

## Consequences

**What this forecloses.**

- No approval by anything other than a person's recorded choice. The AI, a
  timeout, an escalation, a notification link and a scheduled run never approve.
  A scheduler may still propose, and a scheduled proposal is decided by the same
  rules as any other request.
- No approve-all. A session is bounded by site, connection, kind, time and
  count, and there is no bulk approve and no select-all.
- No permission the AI can write. Nothing it can call changes a setting, a
  switch or a session, and nothing it reads or writes changes a decision.
- No way to cover the Ask list by a setting, a session or a connection
  permission.

**What it costs.**

- **An injected AI can now make drafts without a click.** The cost is bounded:
  only the AI's own drafts, each snapshotted and listed in AI activity, each
  with an Undo, and capped per connection per hour. Nothing visitors can see
  runs without a person unless a person turned on Full auto for that site.
- **Existing sites change behavior on release.** Drafts the AI makes run
  without a click. The owners and admins are told by email, and one click on the
  site returns it to Ask every time.
- **Team changes have a cost.** When the person who set a site's setting, or
  allowed a connection, leaves, is demoted, is restricted to some sites or is
  disabled, the changes they stood behind wait for a person until someone
  chooses again. This is deliberate: it is the only way an automatic approval
  stays tied to a person who could still make it.
- **The product sentence changes.** ADR-061 said that when the assistant
  changes your fleet, a named human approved it. It now says that a named person
  either approved that change or chose in advance that this kind of change may
  run, and the record shows which person, which choice and how to undo it.
- **The decline-rate metric applies to Ask cards only.** ADR-061 reads a
  sustained 0% decline rate as a kill signal. That still holds for the cards a
  person is asked to decide. Automatic changes are not in its denominator.
- **The review surface changes.** The AI activity feed, with "Ran
  automatically" and Undo, and from Merge B the Full-auto banner on every page of
  the site, are how a person reviews work they did not click.

**What must be proven before each merge.** Each proof is written to fail
first, and each guard is shown to fire and then not to over-fire: plant the
failure, watch it go red, restore, watch it go green, then run the honest
cases it must not block. Database proofs run as the application role, through
the repository layer, with rows seeded in two tenants.

1. The matrix agrees in code and in the database for every pair of setting and
   class, and flipping one cell turns the test red.
2. Each backstop fires: a request inserted already decided; a person's approval
   with no user or another user; an automatic approval inside a transaction that
   has a user; a raise of a site's setting or a connection's switch with no user
   or a user other than the recorded setter.
3. Each setter check fires, for the site's setter, the connection's setter and
   (Merge B) a session's approver: removal from the organisation, demotion,
   restriction to some sites, a withdrawn share, a disabled account, and a
   failed lookup each make the next request wait.
4. The backfills, run as the production migrator under row security with sites
   seeded in two tenants and read back as the application role, put each site
   and connection where Decision 10 says, including a connection created with a
   key. An empty-database test proves nothing here.
5. A hostile title or page body gives the same decision and the same constant
   message as a plain one.
6. A setting lowered between the decision and the approval, or between the
   approval and the send, stops the request. A class changed after approval
   closes an automatic request unsent and leaves a person's request alone.
7. Each loosening route refuses an API key and an MCP token, and accepts a
   signed-in person with the permission.
8. The draft budget and the draft sites cap send the next request to a person
   with no email.

---

## What takes effect when

| Decision | Merge A | Merge B |
|---|---|---|
| 1. The AI never raises its own permissions | in force for settings and connection switches | adds session approvers |
| 2. Classes and the matrix | the settings Ask every time and Auto for AI drafts; Auto runs `ai_draft` only (page-create, and rest-write on an AI draft whose status is exactly `draft`); everything else waits for a person | the Full auto setting; `operational` Auto, with cache purge on the decider |
| 3. The Ask list | in force | in force |
| 4. Only a signed-in person can loosen | in force for the site setting and the connection switch | adds Full auto and sessions |
| 5. How an approval is recorded | in force for `person` and `policy` | adds `session` |
| 6. Sessions | | in force |
| 7. Budgets | the draft budget and the draft sites cap | the cache, visible-change and organisation caps, and the latch |
| 8. What Undo cannot take back | in force: nothing visitors can see runs without a person | adds the Full-auto confirmation, the read guard and the hold |
| 9. In-client Ask | the link, the closed reasons and the constant messages | `approval: "session"` |
| 10. The default and existing sites | in force, except revoking a removed member's connections, which is its own change after Merge A | |
| 11. Running code | decided, designed separately | decided, designed separately |
