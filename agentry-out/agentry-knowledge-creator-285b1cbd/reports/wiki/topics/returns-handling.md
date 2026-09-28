---
title: Returns handling
type: topic
summary: Deadlines for chilled and dry returns, and the rule that a return is inspected only at the depot that shipped it.
sources: [doc:docs/returns.md; doc:docs/depots.md]
updated: 2026-09-28
---

# Returns handling

Returns are governed by a product-type deadline and a fixed inspection location. [src: doc:docs/returns.md]

## Deadlines

- **Chilled returns** must arrive within 24 hours of dispatch; otherwise they are destroyed. [src: doc:docs/returns.md]
- **Dry returns** are accepted for 30 days. [src: doc:docs/returns.md]

## Inspection location

A return is inspected at the depot that shipped it, never at another depot. This means each depot carries the inspection workload for its own outbound volume rather than routing returns to the nearest site. [src: doc:docs/returns.md]

## Cold-room dependency

Only Rotterdam and Hamburg have cold rooms (40 and 25 pallets respectively); Antwerp has none and Gdansk handles dry goods only. The 24-hour chilled window therefore lands on the two depots with refrigerated storage. [src: doc:docs/depots.md]

The documents do not state whether the 24-hour clock runs from carrier hand-over or from leaving the customer, nor how a chilled return is held if it arrives outside depot hours. [src: doc:docs/returns.md] *(unverified)*

Related: [carriers and lanes](carriers-and-lanes.md) (temperature logs at hand-over), [Rotterdam depot](../entities/rotterdam-depot.md), [Antwerp depot](../entities/antwerp-depot.md).
