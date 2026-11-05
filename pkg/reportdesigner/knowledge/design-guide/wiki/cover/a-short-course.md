---
title: A short course in cover design
type: guide
summary: How to compose a striking report cover on one page - a single idea, hierarchy, a grid, scale contrast, colour blocks, type as image, the logo's place, whitespace - and the mistakes renders show.
sources: [web:https://docs.secureagentry.net/runner/]
updated: 2026-11-05
---

# A short course in cover design

A cover has one job: make the reader want to open the report, and tell them in a glance what it is and who it is from. A striking cover has **one idea**, carried out with discipline [src: web:https://docs.secureagentry.net/runner/].

## One idea

Choose one before you draw: a huge title set as an image of itself; a field of colour with a thin title; a photograph cropped hard with the title on a calm band; a single geometric gesture (a circle, a diagonal, a stripe) in the accent. Two ideas fight. Write your idea in your scratch notes in one sentence, and judge every render against it.

## Hierarchy

The reader's eye should land in this order: the title, then the subtitle, then who it is from, then the date. Make the title much larger than everything else, at least two and a half times the subtitle. Set the quiet facts small, in `facts.muted`, together in one corner.

## A grid

Inset everything by `facts.margin`, or a multiple of it. Align the title, the subtitle and the facts to one left edge. Use the page's thirds: a title at the top third or the bottom third reads as designed; one in the exact centre reads as a default.

## Scale and contrast

Contrast makes a cover striking: very large next to small, heavy next to light, a dark block next to white space. A title at 40 to 60pt in a heavy weight against 9pt facts is not too much. Tighten the tracking of very large type a little (`tracking: -0.5pt`).

## Colour

Use the accent as a large block or a single bold mark, not in many small places. White text sits on the accent only where it is dark enough: `check_design` told you so for the design. Keep the secondary for one supporting shape. Tints make quiet backgrounds.

## Type as image

When there is no picture, the title is the picture. Set it huge, break it over lines yourself (split `facts.title-text` on spaces only to decide sizes, but draw the title once with `facts.title`), and let it bleed toward an edge.

## Images

Crop a photograph to fill a region (`fit: "cover"`), keep text off its busy parts, and put the title on a solid band or a calm area. One image, large, beats several small.

## The logo

Small and calm, in a corner or the brand panel: 12 to 18mm tall. It signs the cover; it is not the subject.

## Whitespace

Empty space is part of the composition. A cover crowded to its edges looks cheap. When in doubt, take something away.

## What the renders show

Look at each render as a stranger would, then name what is wrong before you change code:

- the title wraps badly or crowds an edge: change its size or its box's width;
- text on a colour is hard to read: move it or darken the colour;
- everything is the same size: make the title larger and the rest smaller;
- the facts float in different places: align them to one edge;
- the page looks empty in the wrong place: move the block to a third;
- an image fights the title: crop it or put the title on a band.

Change one or two things a render, so you can see what each change did.
