---
name: calendar-feeds-setup
description: Walk the person through giving you a calendar to read by its iCal (ICS) address, from Google Calendar, Outlook (Microsoft 365 or Outlook.com), iCloud or another calendar service, with no sign-in. Use when the person asks you to look at their calendar and you have no calendar feed for it, or a calendar tool says a feed could not be read.
license: MIT
---

# Set up a calendar feed

Most calendar services publish a calendar at an iCalendar (ICS) address: a
web address that serves the calendar's events to any app that subscribes to
it. Given one, you can read that calendar with your calendar tools, with no
sign-in to the person's account. You cannot do any of this yourself. Each
step is the person's, in their calendar's settings or in Agentry, and you
explain it one step at a time, waiting for them to say it is done.

**Never ask them to paste the address into the chat.** Whoever holds the
address can read the calendar, so it is a secret: it goes to Agentry, which
keeps it in their system keychain, and you are never shown it. If they paste
one anyway, tell them it is now in this conversation, and that they should
reset it in their calendar's settings (Google's and Outlook's have a reset)
and give Agentry the new one.

## What they get, and what they do not

- You read the events the address publishes: titles, times, places, people
  and notes, as the service publishes them. You can say what is on, find free
  time, spot clashes and prepare for a meeting.
- It is **read only**. You cannot add, move, accept or decline an event
  through it. To propose an event, you write an `.ics` file they open (the
  `calendar-feeds` skill says how). If they want you to change their calendar,
  that needs their account signed in as a mail provider instead.
- It is a published copy, and services refresh it on their own schedule:
  often within minutes, sometimes hours later. Say so if they rely on it for
  something just changed.
- They can give you several calendars, each under a name they choose, such as
  "Work" and "Family".

## Find the address (in their calendar)

Ask which service the calendar is in, then give that service's steps.

- **Google Calendar** (on a computer, in a browser): *Settings* (the gear),
  then under *Settings for my calendars* the calendar, then *Integrate
  calendar*, and copy **Secret address in iCal format**. Use the secret
  address, not the public one, unless the calendar is meant to be public. A
  work or school account may not show it: their administrator can turn it
  off. *Reset* beside it makes a new address and stops the old one.
- **Outlook** (Microsoft 365 or Outlook.com, in Outlook on the web):
  *Settings*, then *Calendar*, then *Shared calendars*. Under *Publish a
  calendar*, they choose the calendar and **Can view all details** (or *Can
  view titles and locations*, or *Can view when I'm busy* for free/busy
  only), then *Publish*, and copy the **ICS** link (not the HTML one). A work
  or school account may not offer it: their administrator can turn
  publishing off. *Reset links* there makes new ones.
- **iCloud**: in Calendar on an iPhone or iPad, the *Calendars* list, the
  info button beside the calendar, then turn on **Public Calendar** and *Share
  Link*. The link starts `webcal://`, which is fine. Anyone with the link can
  read it, so it is no more private than the link is kept.
- **Another service** (Fastmail, Proton, Nextcloud, a school or a club's
  calendar): look in the calendar's sharing or publishing settings for an
  "iCal", "ICS", "subscribe" or "webcal" address. A free/busy-only
  publication works too; you then see busy times without titles.

The address must be `https://` or `webcal://`. A plain `http://` one is not
read: most services give the same address with `https://`.

## Give the address to Agentry

Most people use Agentry Desktop: give these steps first. Give all of them
before they stop you, because you cannot answer while you are stopped; your
conversation carries on when they start you again.

### In Agentry Desktop

Agentry Desktop 0.6.176 or later has these; an older version has no card for
calendar feeds, so they use a terminal instead, or update the app.

1. **Stop you**: the **Stop** button at the top of your page.
2. **Open the form**: **Configure**, then the **Mail and calendar** section,
   then **Add a calendar feed**.
3. **The feed**: a **Name** they choose, such as *Work* or *Family*, and the
   address pasted into **Address** (it is not shown), then **Read and keep
   the feed**. Agentry reads the calendar once to check it; an address that
   does not read as a calendar is not kept, and the reason is shown at the top
   of the Configure window. Then it asks them to agree to give it to you, and
   keeps the address in their system keychain.
4. **Start you again**: **Start** at the top of your page.

Each feed then has a card in **Mail and calendar**, with when its address was
checked. A click on the card switches it off and on. Its **⋯** menu has
**Rename** and **Remove**; both, and adding a feed, need you stopped. The same
name given again in **Add a calendar feed** replaces that calendar's address,
after asking.

### In a terminal

If they do not use Agentry Desktop, or its version has no card, the same
steps are commands in a terminal on this computer:

    agentry agent stop assistant
    agentry agent configure assistant --calendar-feed Work

They paste the address when it asks (it is not shown), with the name they
chose in place of `Work`. Agentry reads the calendar once to check it,
says what it found, keeps the address in their system keychain and asks them
to agree to give it to you. Then:

    agentry agent start assistant

A name given again replaces that calendar's address. To take a calendar away:
`agentry agent configure assistant --no-calendar-feed Work`. To keep it but
stop you reading it for a while: `agentry agent grant assistant calendar:Work
off` (and `on` again).

## When it does not work

- "only https is read": the address starts `http://`. They try the same
  address with `https://`.
- "answered HTTP 404" (or 401, 403, 410), when they give it or later in your
  tools: the address is wrong, was reset, or the calendar is no longer
  published. They copy it again from their calendar's settings and give it
  again under the same name.
- "is not a calendar": they copied the HTML link or a page's address, not the
  ICS one.
- Your calendar tools are missing: no feed is on. Their cards in **Mail and
  calendar**, or `agentry agent show assistant`, list the calendar feeds and
  whether each is on.
- An event they just made is not there: the service has not refreshed its
  published copy yet. It usually arrives within the hour.
