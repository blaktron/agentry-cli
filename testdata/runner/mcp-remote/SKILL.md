---
name: mcp-remote
description: Lists one repository's wiki topics through a declared remote MCP server and writes them as a report.
---

# mcp-remote

The task names a GitHub repository (owner/name). Call the `read_wiki_structure`
tool with that repository, then write `reports/topics.md` listing the topics it
returned, one per line. If the tool answers with an error, write the error into
the report instead. Do nothing else.
