---
title: Carriers
type: topic
summary: The three carriers serving the depots, their lanes and schedules, and the temperature log required at hand-over.
sources: [doc:docs/carriers.md]
updated: 2026-09-28
---

# Carriers

Three carriers serve the depot network: two handle chilled goods on fixed weekday lanes and one moves dry goods daily. Every carrier must present a temperature log at hand-over [src: doc:docs/carriers.md].

## Lanes and schedules

- **Nordfreight** carries chilled goods between Rotterdam and Hamburg on Tuesdays and Fridays [src: doc:docs/carriers.md]. See [Rotterdam depot](../entities/rotterdam-depot.md) and [Hamburg depot](../entities/hamburg-depot.md).
- **Van Dijk** carries dry goods daily, the only carrier with no fixed weekday pattern [src: doc:docs/carriers.md].
- **Baltic Reefer** carries chilled goods from Hamburg to Gdansk on Mondays, the only chilled inbound lane into the dry-goods-only [Gdansk depot](../entities/gdansk-depot.md) [src: doc:docs/carriers.md].

## Hand-over rule

Each carrier presents a temperature log when goods are handed over, which is the evidence used when chilled stock arrives and is checked under [returns handling](../procedures/returns-handling.md) [src: doc:docs/carriers.md].
