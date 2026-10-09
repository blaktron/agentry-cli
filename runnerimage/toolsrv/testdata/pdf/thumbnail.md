# Quarterly review

This sample shows how a report design lays out a page: the title block, the
headings, a table, a quote and code. The words are the same in every theme.

## Summary

- Twelve services were reviewed; nine passed without findings.
- Three had findings, one of them **high** severity, now fixed.

> A review says what was checked and what was found.

## Findings

| Service | Severity | Status |
|---|---|---|
| billing-gateway | high | fixed |
| route-planner | medium | in progress |
| warehouse-sync | low | accepted |

### The change

```nginx
ssl_protocols TLSv1.2 TLSv1.3;
```

## Next steps

1. The identity team closes the gap by 31 October.
2. The review team checks the fix and updates this record.
