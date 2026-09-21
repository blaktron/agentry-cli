---
name: mcp-osv
description: Looks up one OSV advisory through the declared MCP server and writes a one-paragraph report.
---

# mcp-osv

The task names an advisory id (a GHSA or a CVE). Call the `osv_get_vulnerability`
tool with that id, then write `reports/advisory.md` with the advisory's summary,
its severity if given, and the affected package names — one paragraph. If the
tool answers with an error, write the error into the report instead. Do nothing
else.
