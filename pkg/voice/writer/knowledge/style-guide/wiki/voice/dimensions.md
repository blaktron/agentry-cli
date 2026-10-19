---
title: The dimensions of a voice
type: guide
summary: What to notice in a person's samples (sentences, words, tone, structure, punctuation and form, registers, what they never do) and how to turn each into an instruction a model can follow.
sources: [web:https://docs.secureagentry.net/agentry/voice/]
updated: 2026-10-19
---

# The dimensions of a voice

A voice is a set of instructions, in the second person, for a model that will write as this person. Each dimension below is something to notice in the samples, then to say as an instruction with a frequency the samples support ("usually", "sometimes", "never") [src: web:https://docs.secureagentry.net/agentry/voice/].

- **Sentences.** How long, and how much the length varies. How they open: with the subject, a clause, a conjunction. Fragments, questions, lists inside a sentence. How often the writer hedges ("I think", "probably") and how. *Instruction:* "Keep most sentences under 15 words. Follow a long one with a short one."
- **Words.** Plain or ornate, the words the writer reaches for and the ones they avoid, jargon, contractions, British or American spelling. *Instruction:* "Use contractions. Say 'use', never 'utilise'."
- **Tone.** Warmth, directness, humour and its kind, confidence, how the writer disagrees and how they praise. *Instruction:* "Disagree directly and say why, then move on."
- **Structure.** How a piece opens and closes, paragraph length, headings, lists or none, how one point leads to the next. *Instruction:* "Open with the conclusion. Give the reason in the next sentence."
- **Punctuation and form.** Dashes, semicolons, exclamation marks, emphasis, emoji, numerals or words for numbers, capitals in headings.
- **Registers.** Where the samples show the writer changing voice, such as a note to a colleague and a public post, say when each applies.
- **Never.** The habits the writer avoids, as plain prohibitions.
- **Examples.** A few short passages in the writer's own words that show the voice at its most typical. Nothing private: no other person's name, no address, number or credential.

Be specific: "Open with the conclusion, then the reason" changes what a model writes, and "Be clear and engaging" does not. Where the samples are too few to tell, leave the point out. Check a habit with `grep` over the samples before you call it "always" or "never".
